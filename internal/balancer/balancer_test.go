package balancer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/siddesh/nimbuslb/internal/backend"
	"github.com/siddesh/nimbuslb/internal/telemetry"
)

// testMetrics builds a Metrics set on a no-op-ish SDK meter via telemetry.Init
// with no OTLP endpoint, so instruments are real but export nowhere harmful.
func testMetrics(t *testing.T) *telemetry.Metrics {
	t.Helper()
	shutdown, m, err := telemetry.Init(t.Context(), "")
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(t.Context()) })
	return m
}

func newPoolWithProxy(t *testing.T, pool *backend.Pool, rawURL string, m *telemetry.Metrics) *backend.Backend {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	b := backend.NewBackend(u, NewReverseProxy(u, pool, m))
	pool.Add(b)
	return b
}

func TestHandlerProxiesToBackend(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("from-upstream"))
	}))
	defer upstream.Close()

	m := testMetrics(t)
	pool := backend.NewPool()
	newPoolWithProxy(t, pool, upstream.URL, m)

	h := NewHandler(pool, m, 3)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://lb/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Result().Body); string(body) != "from-upstream" {
		t.Fatalf("body = %q, want %q", body, "from-upstream")
	}
}

func TestHandlerFailover(t *testing.T) {
	// Dead backend: a listener we immediately close to guarantee conn refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("from-live"))
	}))
	defer live.Close()

	m := testMetrics(t)
	pool := backend.NewPool()
	// Register live first so the first round-robin pick is the dead backend,
	// guaranteeing the failover path is exercised deterministically.
	newPoolWithProxy(t, pool, live.URL, m)
	deadB := newPoolWithProxy(t, pool, deadURL, m)

	h := NewHandler(pool, m, 3)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://lb/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover", rec.Code)
	}
	if deadB.IsAlive() {
		t.Error("dead backend should have been marked down by proxy error")
	}
	if body, _ := io.ReadAll(rec.Result().Body); string(body) != "from-live" {
		t.Fatalf("body = %q, want response from live backend", body)
	}
}

func TestHandlerFailoverOn5xx(t *testing.T) {
	// Broken backend: reachable but always returns 500.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("broken"))
	}))
	defer broken.Close()

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("from-live"))
	}))
	defer live.Close()

	m := testMetrics(t)
	pool := backend.NewPool()
	// Live first so the first round-robin pick is the broken backend.
	newPoolWithProxy(t, pool, live.URL, m)
	brokenB := newPoolWithProxy(t, pool, broken.URL, m)

	h := NewHandler(pool, m, 3)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://lb/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after 5xx failover", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Result().Body); string(body) != "from-live" {
		t.Fatalf("body = %q, want response from live backend", body)
	}
	if brokenB.IsAlive() {
		t.Error("backend returning 5xx should have been marked down")
	}
}

func TestHandlerNoHealthyBackends(t *testing.T) {
	m := testMetrics(t)
	pool := backend.NewPool()
	b := newPoolWithProxy(t, pool, "http://127.0.0.1:1", m)
	b.SetAlive(false)

	h := NewHandler(pool, m, 3)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://lb/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
