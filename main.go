package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	rlmiddleware "github.com/h-hmz/rate-limiter/middleware"
)

func main() {
	ctx := context.Background()
	if err := run(ctx, os.Stdout, os.Args); err != nil {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, w io.Writer, _ []string) error {
	setupLogging(w)

	// Listen for both SIGINT (Ctrl+C) and SIGTERM (Docker/Kubernetes)
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	limiter, closeStore, err := buildLimiter(ctx, cfg)
	if err != nil {
		return err
	}
	if closeStore != nil {
		// Deferred so it runs after Shutdown has drained in-flight requests.
		defer func() {
			if err := closeStore(); err != nil {
				slog.Warn("closing store failed", "err", err)
			}
		}()
	}

	reverseProxy := newReverseProxy(cfg.AppPort)
	extractor := buildExtractor(cfg)

	proxySrv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.ListenPort),
		Handler: rlmiddleware.HttpMiddleware(
			limiter,
			extractor,
		)(reverseProxy),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
	}

	srvErr := make(chan error, 1)

	go func() {
		slog.Info("listening",
			"addr", proxySrv.Addr,
			"app_port", cfg.AppPort,
			"algorithm", cfg.Algorithm,
			"store", cfg.Store,
			"key", cfg.Key,
			"fail_open", cfg.FailOpen,
		)
		if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	select {
	case err := <-srvErr:
		return fmt.Errorf("server startup failed: %w", err)
	case <-ctx.Done():
		slog.Info("shutdown signal received")

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := proxySrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("error during server shutdown: %w", err)
		}
	}

	return nil
}
