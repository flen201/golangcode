package main

import (
   "context"
   "fmt"
   "log"
   "os"
   "os/signal"
   "path/filepath"
   "strings"
   "syscall"
   "time"
   
   "mailer/internal/config"
   "mailer/internal/database"
   "mailer/internal/observability"
   "mailer/internal/smtp"
   "mailer/internal/email"
   "mailer/internal/cli"
   "mailer/internal/campaign"
)

const (
   AppName    = "Mailer"
   AppVersion = "2.0.0"
   GoVersion  = "1.21"
)

func main() {
   cfg, err := config.LoadFromStandardLocations()
   if err != nil {
       log.Fatalf("Config not found. Create config.yaml in current directory or config/config.yaml\nError: %v", err)
   }
   
   logger, err := observability.NewLogger(cfg.Logging)
   if err != nil {
       log.Fatalf("Failed to initialize logger: %v", err)
   }
   defer logger.Sync()
   
   ctx := context.Background()
   logger.Info(ctx, "Starting Mailer",
       "version", AppVersion,
       "environment", cfg.App.Environment,
   )
   
   db, err := database.New(cfg.Database, logger)
   if err != nil {
       logger.Error(ctx, "Failed to initialize database", "error", err)
       os.Exit(1)
   }
   defer db.Close()
   
   smtpManager, err := smtp.NewEnterpriseManager(&cfg.SMTP, logger)
   if err != nil {
       logger.Error(ctx, "Failed to initialize SMTP manager", "error", err)
       os.Exit(1)
   }
   defer smtpManager.Close()
   
   templateEngine := email.NewEnterpriseTemplateEngine(logger)
   
   if err := loadTemplates(templateEngine, logger); err != nil {
       logger.Warn(ctx, "Failed to load some templates", "error", err)
   }
   
   processor := email.NewEnterpriseProcessor(
       &cfg.Processor,
       logger,
       db,
       smtpManager,
       templateEngine,
   )
   
   if err := processor.Start(); err != nil {
       logger.Error(ctx, "Failed to start email processor", "error", err)
       os.Exit(1)
   }
   defer processor.Stop()
   
   campaignManager := campaign.NewManager(logger, db, processor, templateEngine)
   defer campaignManager.Close()
   
   shutdown := make(chan os.Signal, 1)
   signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
   
   logger.Info(ctx, "Mailer ready")
   
   cliInterface := cli.NewInterface(
       cfg,
       logger,
       db,
       processor,
       smtpManager,
   )
   
   cliDone := make(chan error, 1)
   go func() {
       cliDone <- cliInterface.Run()
   }()
   
   select {
   case <-shutdown:
       logger.Info(ctx, "Shutdown signal received")
   case err := <-cliDone:
       if err != nil {
           logger.Error(ctx, "CLI interface error", "error", err)
       } else {
           logger.Info(ctx, "CLI completed normally")
       }
   }
   
   logger.Info(ctx, "Shutting down gracefully...")
   shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
   defer cancel()
   
   if err := gracefulShutdown(shutdownCtx, logger, processor, smtpManager, campaignManager, db); err != nil {
       logger.Error(ctx, "Error during shutdown", "error", err)
       os.Exit(1)
   }
   
   logger.Info(ctx, "Mailer stopped")
}

func loadTemplates(templateEngine *email.EnterpriseTemplateEngine, logger *observability.Logger) error {
   templatesDir := "templates"
   
   files, err := os.ReadDir(templatesDir)
   if err != nil {
       return fmt.Errorf("failed to read templates directory: %w", err)
   }
   
   for _, file := range files {
       if file.IsDir() || !strings.HasSuffix(file.Name(), ".html") {
           continue
       }
       
       templatePath := filepath.Join(templatesDir, file.Name())
       content, err := os.ReadFile(templatePath)
       if err != nil {
           logger.Warn(context.Background(), "Failed to load template",
               "template", file.Name(),
               "error", err,
           )
           continue
       }
       
       templateName := strings.TrimSuffix(file.Name(), ".html")
       if err := templateEngine.LoadTemplate(templateName, string(content), nil); err != nil {
           logger.Warn(context.Background(), "Failed to register template",
               "template", templateName,
               "error", err,
           )
           continue
       }
       
       logger.Debug(context.Background(), "Template loaded",
           "template", templateName,
       )
   }
   
   return nil
}

func gracefulShutdown(ctx context.Context, logger *observability.Logger, processor *email.EnterpriseProcessor, smtpManager *smtp.EnterpriseManager, campaignManager *campaign.Manager, db *database.Database) error {
   logger.Info(ctx, "Starting graceful shutdown")
   
   if processor != nil {
       logger.Info(ctx, "Stopping email processor")
       if err := processor.Stop(); err != nil {
           logger.Error(ctx, "Error stopping processor", "error", err)
       }
   }
   
   if campaignManager != nil {
       logger.Info(ctx, "Stopping campaign manager")
       if err := campaignManager.Close(); err != nil {
           logger.Error(ctx, "Error stopping campaign manager", "error", err)
       }
   }
   
   if smtpManager != nil {
       logger.Info(ctx, "Closing SMTP connections")
       if err := smtpManager.Close(); err != nil {
           logger.Error(ctx, "Error closing SMTP manager", "error", err)
       }
   }
   
   if db != nil {
       logger.Info(ctx, "Closing database connections")
       if err := db.Close(); err != nil {
           logger.Error(ctx, "Error closing database", "error", err)
       }
   }
   
   logger.Info(ctx, "Graceful shutdown completed")
   return nil
}