// Command nimbuslb runs the NimbusLB HTTP load balancer.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/siddesh/nimbuslb/internal/backend"
	"github.com/siddesh/nimbuslb/internal/balancer"
	"github.com/siddesh/nimbuslb/internal/config"
	"github.com/siddesh/nimbuslb/internal/health"
	"github.com/siddesh/nimbuslb/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Telemetry (traces via OTLP, metrics via Prometheus exporter).
	shutdownTel, metrics, err := telemetry.Init(ctx, cfg.OTLPEndpoint)
	if err != nil {
		return fmt.Errorf("telemetry init: %w", err)
	}
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTel(shCtx)
	}()

	// Build the pool and one instrumented reverse proxy per backend.
	pool := backend.NewPool()
	for _, raw := range cfg.Backends {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid backend %q: %w", raw, err)
		}
		proxy := balancer.NewReverseProxy(u, pool, metrics)
		pool.Add(backend.NewBackend(u, proxy))
		slog.Info("registered backend", "url", u.String())
	}

	// Active health checks.
	checker := health.NewChecker(pool, cfg.HealthInterval, cfg.HealthTimeout)
	checker.CheckNow(ctx) // seed initial state
	checker.Start(ctx)

	// Metrics server (separate listener, no tracing middleware).
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", telemetry.MetricsHandler())
	metricsSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.MetricsPort),
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		slog.Info("metrics listening", "addr", metricsSrv.Addr)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server failed", "error", err)
		}
	}()

	// Main proxy server: wrap with otelhttp for tracing + inflight tracking.
	handler := balancer.NewHandler(pool, metrics, cfg.Retries)
	traced := otelhttp.NewHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			metrics.InflightInc(r.Context())
			defer metrics.InflightDec(r.Context())
			r = r.WithContext(telemetry.ContextWithStartTime(r.Context()))
			handler.ServeHTTP(w, r)
		}),
		"lb.request",
	)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           traced,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("load balancer listening", "addr", srv.Addr, "backends", len(cfg.Backends))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down gracefully")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shCtx)
		return srv.Shutdown(shCtx)
	}
}
