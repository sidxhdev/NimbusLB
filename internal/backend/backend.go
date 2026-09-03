// Package backend provides a thread-safe pool of upstream backends and
// round-robin peer selection for the load balancer.
package backend

import (
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
)

// Backend represents a single upstream server that traffic can be proxied to.
// Its liveness flag is updated concurrently by the health checker and by the
// proxy's error handler, so it is stored in an atomic.Bool.
type Backend struct {
	URL   *url.URL
	Alive atomic.Bool
	Proxy *httputil.ReverseProxy
}

// NewBackend creates a Backend for the given upstream URL. Backends start in
// the alive state and are demoted by health checks or proxy errors.
func NewBackend(u *url.URL, proxy *httputil.ReverseProxy) *Backend {
	b := &Backend{URL: u, Proxy: proxy}
	b.Alive.Store(true)
	return b
}

// SetAlive marks the backend as alive (healthy) or dead (unreachable).
func (b *Backend) SetAlive(alive bool) { b.Alive.Store(alive) }

// IsAlive reports whether the backend is currently considered healthy.
func (b *Backend) IsAlive() bool { return b.Alive.Load() }

// Pool is a thread-safe registry of backends with lock-free round-robin
// selection across the healthy members.
type Pool struct {
	mu       sync.RWMutex
	backends []*Backend
	current  atomic.Uint64
}

// NewPool returns an empty Pool.
func NewPool() *Pool { return &Pool{} }

// Add registers a backend with the pool.
func (p *Pool) Add(b *Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends = append(p.backends, b)
}

// Backends returns a snapshot of all registered backends.
func (p *Pool) Backends() []*Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Backend, len(p.backends))
	copy(out, p.backends)
	return out
}

// NextPeer returns the next healthy backend in round-robin order. It scans
// the full pool at most once, skipping dead backends, and returns nil when
// every backend is unhealthy.
func (p *Pool) NextPeer() *Backend {
	backends := p.Backends()
	n := len(backends)
	if n == 0 {
		return nil
	}
	start := p.current.Add(1)
	for i := uint64(0); i < uint64(n); i++ {
		b := backends[(start+i)%uint64(n)]
		if b.IsAlive() {
			return b
		}
	}
	return nil
}

// MarkStatus sets the liveness of the backend identified by rawURL. It
// returns true when the status actually changed (i.e. a transition occurred).
func (p *Pool) MarkStatus(rawURL string, alive bool) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, b := range p.backends {
		if b.URL.String() == rawURL {
			if b.IsAlive() != alive {
				b.SetAlive(alive)
				return true
			}
			return false
		}
	}
	return false
}
