// Package server exposes health, status, metrics and the sync trigger over HTTP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Options configures the HTTP server.
type Options struct {
	Addr     string
	Logger   *slog.Logger
	Registry *prometheus.Registry
	// Ready reports whether dependencies (S3, ...) are usable. Nil means always ready.
	Ready func(ctx context.Context) error
	// Status returns the current state of every repo (JSON-encoded as is).
	Status func() any
	// Trigger starts a sync of the named repo. It returns ErrBusy-like errors
	// mapped through TriggerStatus.
	Trigger func(repo string) error
	// TriggerStatus maps a Trigger error to an HTTP status code.
	TriggerStatus func(err error) int
}

// Handler builds the HTTP routes.
func Handler(o Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if o.Ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := o.Ready(ctx); err != nil {
				o.Logger.Warn("readiness check failed", "error", err)
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		var repos any = []any{}
		if o.Status != nil {
			repos = o.Status()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"repos": repos})
	})
	if o.Trigger != nil {
		mux.HandleFunc("POST /sync/{repo}", func(w http.ResponseWriter, r *http.Request) {
			name := r.PathValue("repo")
			if err := o.Trigger(name); err != nil {
				code := http.StatusInternalServerError
				if o.TriggerStatus != nil {
					code = o.TriggerStatus(err)
				}
				http.Error(w, err.Error(), code)
				return
			}
			o.Logger.Info("sync triggered via API", "repo", name)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("accepted\n"))
		})
	}
	mux.Handle("GET /metrics", promhttp.HandlerFor(o.Registry, promhttp.HandlerOpts{}))
	return mux
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, o Options) error {
	srv := &http.Server{
		Addr:              o.Addr,
		Handler:           Handler(o),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		o.Logger.Info("http server listening", "addr", o.Addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
