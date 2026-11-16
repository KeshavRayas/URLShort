package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kesha/urlshortener/internal/analytics"
	"github.com/kesha/urlshortener/internal/auth"
	"github.com/kesha/urlshortener/internal/config"
	"github.com/kesha/urlshortener/internal/httpapi"
	"github.com/kesha/urlshortener/internal/link"
	"github.com/kesha/urlshortener/internal/ratelimit"
	"github.com/kesha/urlshortener/internal/storage/postgres"
	"github.com/kesha/urlshortener/internal/storage/rediscache"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	if err := run(cfg, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := postgres.Migrate(ctx, pool); err != nil {
		return err
	}

	seeds := make([]postgres.SeedUser, 0, len(cfg.BootstrapUsers))
	for _, u := range cfg.BootstrapUsers {
		seeds = append(seeds, postgres.SeedUser{
			Name:    u.Name,
			KeyHash: auth.HashKey(cfg.APIKeyPepper, u.APIKey),
		})
	}
	if err := postgres.BootstrapUsers(ctx, pool, seeds); err != nil {
		return err
	}

	rdb, err := rediscache.Connect(ctx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, log)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	store := postgres.NewStore(pool)
	cache := rediscache.NewCache(rdb)

	recorder := analytics.New(cfg.ClickBuffer, store, log)
	recorder.Start()
	defer func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		recorder.Close(drainCtx)
	}()

	links := link.NewService(link.Options{
		Store:               store,
		Cache:               cache,
		BaseHost:            cfg.PublicBaseURL.Hostname(),
		CacheTTL:            cfg.CacheTTL,
		NegativeTTL:         cfg.NegativeCacheTTL,
		AllowPrivateTargets: cfg.AllowPrivateTargets,
		Logger:              log,
	})

	api := httpapi.New(httpapi.Options{
		Config:  cfg,
		Links:   links,
		Users:   store,
		Limiter: ratelimit.New(rdb, cfg.RateLimitPerMinute),
		Clicks:  recorder,
		Ready:   func(ctx context.Context) error { return pool.Ping(ctx) },
		Logger:  log,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr, "base_url", cfg.PublicBaseURL.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	log.Info("server stopped")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
