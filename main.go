package main

import (
	"context"
	"fmt"
	"io"
	"log"
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
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, w io.Writer, args []string) error {
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
		// Deferred so it runs after Shutdown has drained in-flight requestss.
		defer func() {
			if err := closeStore(); err != nil {
				log.Printf("closing store: %v", err)
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
		log.Printf("listening on %s", proxySrv.Addr)
		if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	select {
	case err := <-srvErr:
		return fmt.Errorf("server startup failed: %w", err)
	case <-ctx.Done():
		log.Println("Interrupt signal received, initiating graceful shutdown...")

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := proxySrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("error during server shutdown: %w", err)
		}
	}

	return nil
}
