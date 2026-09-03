package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	rlmetrics "github.com/h-hmz/rate-limiter/metrics"
	rlmiddleware "github.com/h-hmz/rate-limiter/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = time.Minute
	shutdownTimeout         = 10 * time.Second
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

	slog.Info("starting",
		"app_port", cfg.AppPort,
		"algorithm", cfg.Algorithm,
		"store", cfg.Store,
		"key", cfg.Key,
		"fail_open", cfg.FailOpen,
	)

	exporter, err := promexporter.New()
	if err != nil {
		return fmt.Errorf("prometheus exporter: %w", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(provider)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := provider.Shutdown(shutdownCtx); err != nil {
			slog.Warn("shutting down meter provider failed", "err", err)
		}
	}()

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

	instrumented, err := rlmetrics.New(limiter)
	if err != nil {
		return fmt.Errorf("instrumenting limiter: %w", err)
	}

	reverseProxy := newReverseProxy(cfg.AppPort)
	extractor := buildExtractor(cfg)

	proxySrv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.ListenPort),
		Handler: rlmiddleware.HttpMiddleware(
			instrumented,
			extractor,
		)(reverseProxy),
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

	// Admin endpoints get their own listener so it:
	// - doesn't shadow the app's own /metrics route
	// - isn't subject to rate limiting
	adminMux := http.NewServeMux()
	adminMux.Handle("/metrics", promhttp.Handler())
	adminMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Liveness only: the process is up and serving. Store health is deliberately
		// excluded since a failed store results in a fail open.
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	adminSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.MetricsPort),
		Handler:           adminMux,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

	proxyLn, err := net.Listen("tcp", proxySrv.Addr)
	if err != nil {
		return fmt.Errorf("proxy listener: %w", err)
	}
	adminLn, err := net.Listen("tcp", adminSrv.Addr)
	if err != nil {
		proxyLn.Close()
		return fmt.Errorf("admin listener: %w", err)
	}

	srvErr := make(chan error, 2)
	serve := func(name string, srv *http.Server, ln net.Listener) {
		slog.Info("listening", "server", name, "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("%s server: %w", name, err)
		}
	}
	go serve("proxy", proxySrv, proxyLn)
	go serve("admin", adminSrv, adminLn)

	select {
	case err := <-srvErr:
		return err

	case <-ctx.Done():
		slog.Info("shutdown signal received")

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer shutdownCancel()

		// Drain the proxy first then the admin server so a scrape during shutdown still works.
		err := proxySrv.Shutdown(shutdownCtx)
		if adminErr := adminSrv.Shutdown(shutdownCtx); err == nil {
			err = adminErr
		}
		if err != nil {
			return fmt.Errorf("error during server shutdown: %w", err)
		}
	}

	return nil
}
