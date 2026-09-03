// Package health implements active health checking of pool backends.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/siddesh/nimbuslb/internal/backend"
	"github.com/siddesh/nimbuslb/internal/telemetry"
)

// Checker periodically probes every backend's /healthz endpoint and updates
// its liveness in the pool. A backend that was passively marked dead by the
// proxy is automatically restored once a probe succeeds again.
type Checker struct {
	pool     *backend.Pool
	interval time.Duration
	timeout  time.Duration
	client   *http.Client
}

// NewChecker creates a Checker probing every interval with per-probe timeout.
func NewChecker(pool *backend.Pool, interval, timeout time.Duration) *Checker {
	return &Checker{
		pool:     pool,
		interval: interval,
		timeout:  timeout,
		client:   &http.Client{Timeout: timeout},
	}
}

// Start launches the periodic probing loop in a background goroutine. It
// returns immediately; the loop stops when ctx is cancelled.
func (c *Checker) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.checkAll(ctx)
			}
		}
	}()
}

// checkAll probes every registered backend once. Exported as CheckNow for
// tests and one-shot verification.
func (c *Checker) checkAll(ctx context.Context) { c.CheckNow(ctx) }

// CheckNow runs a single probe round across all backends.
func (c *Checker) CheckNow(ctx context.Context) {
	for _, b := range c.pool.Backends() {
		alive := c.probe(ctx, b.URL.String())
		if c.pool.MarkStatus(b.URL.String(), alive) {
			if alive {
				slog.Info("backend recovered", "backend", b.URL.String())
			} else {
				slog.Warn("backend health check failed, marked DOWN", "backend", b.URL.String())
			}
		}
		telemetry.SetBackendHealth(b.URL.String(), alive)
	}
}

// probe issues GET /healthz and treats any 2xx as healthy.
func (c *Checker) probe(ctx context.Context, base string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
