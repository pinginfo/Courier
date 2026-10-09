package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"courier/internal/observability"
)

func newHandler(ready func(context.Context) error) http.Handler {
	m := observability.NewMetrics("test")
	m.InitRepo("alpine")
	return Handler(Options{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry: m.Registry,
		Ready:    ready,
		Status:   func() []RepoStatus { return []RepoStatus{{Name: "alpine", Type: "apk", Schedule: "* * * * *"}} },
	})
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	if rec := get(t, newHandler(nil), "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	if rec := get(t, newHandler(nil), "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("ready code = %d", rec.Code)
	}
	failing := newHandler(func(context.Context) error { return errors.New("s3 down") })
	if rec := get(t, failing, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready code = %d", rec.Code)
	}
}

func TestStatus(t *testing.T) {
	rec := get(t, newHandler(nil), "/status")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"alpine"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestMetrics(t *testing.T) {
	rec := get(t, newHandler(nil), "/metrics")
	body := rec.Body.String()
	for _, want := range []string{
		`courier_sync_last_success_timestamp_seconds{repo="alpine"} 0`,
		`courier_sync_runs_total{repo="alpine",result="failure"} 0`,
		`courier_build_info{version="test"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestRunShutsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := observability.NewMetrics("test")
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Addr: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Registry: m.Registry})
	}()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
