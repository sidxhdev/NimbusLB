// Package balancer implements the HTTP handler that routes incoming requests
// to healthy backends via reverse proxies, with bounded retry/failover.
package balancer

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/siddesh/nimbuslb/internal/backend"
	"github.com/siddesh/nimbuslb/internal/telemetry"
)

// Handler proxies requests to the pool using round-robin selection and
// retries failed attempts on other healthy backends.
type Handler struct {
	pool    *backend.Pool
	metrics *telemetry.Metrics
	retries int
}

// NewHandler builds the balancer handler. retries bounds how many backend
// attempts a single request may make before returning 503.
func NewHandler(pool *backend.Pool, m *telemetry.Metrics, retries int) *Handler {
	if retries < 1 {
		retries = 1
	}
	return &Handler{pool: pool, metrics: m, retries: retries}
}

// ServeHTTP routes the request through healthy backends, failing over on
// transport errors and upstream 5xx responses until retries are exhausted.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for attempt := 1; attempt <= h.retries; attempt++ {
		peer := h.pool.NextPeer()
		if peer == nil {
			h.metrics.RecordError(r.Context(), "no_healthy_backends", "none")
			http.Error(w, "503 Service Unavailable: no healthy backends", http.StatusServiceUnavailable)
			return
		}

		buf := &responseBuffer{header: make(http.Header), status: http.StatusOK}
		peer.Proxy.ServeHTTP(buf, r)

		switch {
		case buf.failed:
			// Transport error: proxy ErrorHandler already demoted the
			// backend and recorded the error metric. Try the next peer.
			slog.Info("failing over after transport error",
				"backend", peer.URL.String(), "attempt", attempt+1)
			continue
		case buf.status >= 500:
			// Alive but broken upstream: demote and try the next peer.
			h.markDown(peer)
			h.metrics.RecordError(r.Context(), "backend_5xx", peer.URL.String())
			slog.Info("failing over after server error",
				"backend", peer.URL.String(), "status", buf.status, "attempt", attempt+1)
			continue
		default:
			h.metrics.RecordRequest(r.Context(), peer.URL.String(), r.Method, r.URL.Path, buf.status)
			buf.flush(w)
			return
		}
	}

	// All attempts exhausted on server errors: report the last failure.
	h.metrics.RecordError(r.Context(), "retries_exhausted", "all")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "503 Service Unavailable: all retries exhausted\n")
}

// markDown demotes a backend and surfaces the transition in logs + metrics.
func (h *Handler) markDown(b *backend.Backend) {
	if h.pool.MarkStatus(b.URL.String(), false) {
		slog.Info("backend marked DOWN", "backend", b.URL.String())
	}
	telemetry.SetBackendHealth(b.URL.String(), false)
}

// responseBuffer implements http.ResponseWriter, capturing the complete
// upstream response so server-error attempts can be discarded and retried
// before anything reaches the client.
type responseBuffer struct {
	header http.Header
	body   bytes.Buffer
	status int
	failed bool // set by the proxy ErrorHandler on transport errors
}

func (b *responseBuffer) Header() http.Header { return b.header }

func (b *responseBuffer) WriteHeader(status int) { b.status = status }

func (b *responseBuffer) Write(p []byte) (int, error) { return b.body.Write(p) }

// flush copies the buffered response to the real ResponseWriter.
func (b *responseBuffer) flush(w http.ResponseWriter) {
	for k, vs := range b.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(b.status)
	_, _ = io.Copy(w, &b.body)
}

// NewReverseProxy creates a reverse proxy for target with an OTel-instrumented
// transport and an error handler that demotes unreachable backends.
func NewReverseProxy(target *url.URL, pool *backend.Pool, m *telemetry.Metrics) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = otelhttp.NewTransport(http.DefaultTransport)

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Preserve the original host for backends that care about it.
		if req.Header.Get("X-Forwarded-Host") == "" {
			req.Header.Set("X-Forwarded-Host", req.Host)
		}
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		backendURL := target.String()
		slog.Warn("backend unreachable", "backend", backendURL, "error", err)
		if pool.MarkStatus(backendURL, false) {
			slog.Info("backend marked DOWN", "backend", backendURL)
		}
		telemetry.SetBackendHealth(backendURL, false)
		m.RecordError(r.Context(), "proxy_error", backendURL)

		// Signal the balancer to fail over; it owns the client response.
		if buf, ok := w.(*responseBuffer); ok {
			buf.failed = true
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "503 Service Unavailable: all retries exhausted\n")
	}

	return proxy
}
