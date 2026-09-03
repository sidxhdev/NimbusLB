package backend

import (
	"fmt"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func newTestBackend(t *testing.T, raw string) *Backend {
	t.Helper()
	return NewBackend(mustURL(t, raw), &httputil.ReverseProxy{})
}

func TestRoundRobinDistribution(t *testing.T) {
	p := NewPool()
	for i := 1; i <= 3; i++ {
		p.Add(newTestBackend(t, fmt.Sprintf("http://b%d", i)))
	}

	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		b := p.NextPeer()
		if b == nil {
			t.Fatal("NextPeer returned nil with healthy backends")
		}
		counts[b.URL.Host]++
	}
	for host, n := range counts {
		if n != 100 {
			t.Errorf("backend %s got %d requests, want 100", host, n)
		}
	}
}

func TestNextPeerSkipsDead(t *testing.T) {
	p := NewPool()
	dead := newTestBackend(t, "http://dead")
	live := newTestBackend(t, "http://live")
	p.Add(dead)
	p.Add(live)
	dead.SetAlive(false)

	for i := 0; i < 50; i++ {
		if got := p.NextPeer(); got != live {
			t.Fatalf("NextPeer = %v, want live backend", got.URL)
		}
	}
}

func TestNextPeerAllDead(t *testing.T) {
	p := NewPool()
	b := newTestBackend(t, "http://b1")
	p.Add(b)
	b.SetAlive(false)
	if got := p.NextPeer(); got != nil {
		t.Fatalf("NextPeer = %v, want nil when all backends dead", got.URL)
	}
}

func TestMarkStatusTransitions(t *testing.T) {
	p := NewPool()
	b := newTestBackend(t, "http://b1")
	p.Add(b)

	if !p.MarkStatus("http://b1", false) {
		t.Error("expected transition on first dead mark")
	}
	if p.MarkStatus("http://b1", false) {
		t.Error("expected no transition when already dead")
	}
	if !p.MarkStatus("http://b1", true) {
		t.Error("expected transition on recovery")
	}
	if p.MarkStatus("http://unknown", true) {
		t.Error("expected no transition for unknown backend")
	}
}

func TestPoolConcurrentAccess(t *testing.T) {
	p := NewPool()
	for i := 0; i < 10; i++ {
		p.Add(newTestBackend(t, fmt.Sprintf("http://b%d", i)))
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func(i int) { // selector
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = p.NextPeer()
			}
		}(i)
		go func(i int) { // status flapper
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.MarkStatus(fmt.Sprintf("http://b%d", i%10), j%2 == 0)
			}
		}(i)
		go func() { // snapshot reader
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = p.Backends()
			}
		}()
	}
	wg.Wait()
}
