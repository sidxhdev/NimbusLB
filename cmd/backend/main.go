// Command backend is a demo upstream server for NimbusLB. It identifies
// itself in responses and exposes /fail and /recover toggles to demonstrate
// health-check-driven failover.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
)

func main() {
	port := flag.Int("port", 8081, "listen port")
	id := flag.String("id", "backend", "server identity returned in responses")
	flag.Parse()

	var healthy atomic.Bool
	healthy.Store(true)

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "500 failing", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "hello from %s (path=%s)\n", *id, r.URL.Path)
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		healthy.Store(false)
		slog.Info("marked unhealthy", "id", *id)
		fmt.Fprintln(w, "now failing")
	})

	mux.HandleFunc("/recover", func(w http.ResponseWriter, r *http.Request) {
		healthy.Store(true)
		slog.Info("marked healthy", "id", *id)
		fmt.Fprintln(w, "now healthy")
	})

	addr := fmt.Sprintf(":%d", *port)
	slog.Info("demo backend listening", "addr", addr, "id", *id)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server failed", "error", err)
	}
}
