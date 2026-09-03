package health

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siddesh/nimbuslb/internal/backend"
)

func addBackend(t *testing.T, pool *backend.Pool, raw string) *backend.Backend {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := backend.NewBackend(u, &httputil.ReverseProxy{})
	pool.Add(b)
	return b
}

func TestCheckerMarksDeadAndRecovers(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	pool := backend.NewPool()
	b := addBackend(t, pool, srv.URL)

	c := NewChecker(pool, time.Hour, 2*time.Second) // manual CheckNow only

	c.CheckNow(t.Context())
	if !b.IsAlive() {
		t.Fatal("healthy backend was marked dead")
	}

	healthy.Store(false)
	c.CheckNow(t.Context())
	if b.IsAlive() {
		t.Fatal("failing backend was not marked dead")
	}

	healthy.Store(true)
	c.CheckNow(t.Context())
	if !b.IsAlive() {
		t.Fatal("recovered backend was not marked alive")
	}
}

func TestCheckerUnreachableBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	pool := backend.NewPool()
	b := addBackend(t, pool, deadURL)

	c := NewChecker(pool, time.Hour, 500*time.Millisecond)
	c.CheckNow(t.Context())
	if b.IsAlive() {
		t.Fatal("unreachable backend was not marked dead")
	}
}
