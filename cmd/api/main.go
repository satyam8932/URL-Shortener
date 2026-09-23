package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"url_shortener/internal/config"
	"url_shortener/internal/database"
	"url_shortener/internal/handler"
	"url_shortener/internal/logging"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadAPI(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := logging.New(os.Stdout, "api", cfg.LogLevel)
	slog.SetDefault(logger)

	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("close database", slog.Any("error", err))
		}
	}()

	server := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           handler.NewRouter(handler.Dependencies{Logger: logger, DB: db}),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		logger.Info("http server listening", slog.String("addr", cfg.HTTP.Addr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve http: %w", err)
		}
		return nil
	})

	group.Go(func() error {
		<-groupCtx.Done()
		stop()
		logger.Info("shutting down", slog.String("timeout", cfg.HTTP.ShutdownTimeout.String()))

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown http server: %w", err)
		}
		return nil
	})

	if err := group.Wait(); err != nil {
		return err
	}

	logger.Info("shutdown complete")
	return nil
}
