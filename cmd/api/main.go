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

	"url_shortener/internal/cache"
	"url_shortener/internal/config"
	"url_shortener/internal/database"
	"url_shortener/internal/handler"
	"url_shortener/internal/logging"
	"url_shortener/internal/ratelimit"
	"url_shortener/internal/repository"
	"url_shortener/internal/service"
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
	client := database.NewClient(db)
	defer func() {
		if err := client.Close(); err != nil {
			logger.Error("close database", slog.Any("error", err))
		}
	}()

	redisClient, err := database.OpenRedis(cfg.Redis)
	if err != nil {
		return fmt.Errorf("open redis: %w", err)
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			logger.Error("close redis", slog.Any("error", err))
		}
	}()
	// Redis is optional at runtime, so an outage at startup is not fatal: the
	// cache falls back to Postgres and rate limiting fails open until it returns.
	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Warn("redis unreachable, running without cache and rate limiting", slog.Any("error", err))
	}

	store := repository.NewLinks(client)
	links := service.NewLinks(store, cache.NewLinks(redisClient), cfg.CacheTTL, logger)
	clicks := service.NewClickCounter(store, logger)
	sweeper := service.NewExpirySweeper(store, cfg.ExpirySweepInterval, logger)
	limiter := ratelimit.New(redisClient, cfg.RateLimit.PerMinute, cfg.RateLimit.Burst)

	server := &http.Server{
		Addr: cfg.HTTP.Addr,
		Handler: handler.NewRouter(handler.Dependencies{
			Logger:         logger,
			DB:             db,
			Links:          links,
			Clicks:         clicks,
			Limiter:        limiter,
			ClientIPHeader: cfg.RateLimit.ClientIPHeader,
			PublicBaseURL:  cfg.PublicBaseURL,
			AdminToken:     cfg.AdminToken,
			RequestTimeout: cfg.HTTP.WriteTimeout,
		}),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Workers get their own context, cancelled only after the server has
	// drained, so clicks recorded by in-flight requests still get flushed.
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		clicks.Run(workerCtx)
		return nil
	})
	group.Go(func() error {
		sweeper.Run(workerCtx)
		return nil
	})

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

		err := server.Shutdown(shutdownCtx)
		stopWorkers()
		if err != nil {
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
