// Command migrate brings the database schema in line with the ent schema
// definitions in ent/schema, then exits.
//
// It runs as its own step, before the API starts, rather than inside the API
// process: deploys then never race several API instances migrating at once,
// and a failed migration stops the rollout instead of crash-looping the
// server. Changes are additive only; ent never drops columns or indexes
// unless explicitly told to.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"url_shortener/internal/config"
	"url_shortener/internal/database"
	"url_shortener/internal/logging"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migration failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadMigrate(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := logging.New(os.Stdout, "migrate", cfg.LogLevel)
	slog.SetDefault(logger)

	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	client := database.NewClient(db)
	defer func() {
		if err := client.Close(); err != nil {
			logger.Error("close database", slog.Any("error", err))
		}
	}()

	logger.Info("applying schema")
	if err := client.Schema.Create(ctx); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	logger.Info("schema is up to date")

	return nil
}
