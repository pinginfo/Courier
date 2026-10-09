package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"courier/internal/config"
	"courier/internal/observability"
	"courier/internal/storage"
)

func testRunner(t *testing.T) *Runner {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Concurrency: 2},
		Repos: []config.Repo{{
			Name: "ubuntu", Type: config.TypeAPT, Upstream: "https://example.org", Prefix: "ubuntu", Schedule: "0 4 * * *",
			APT: &config.APT{Suites: []string{"noble"}, Components: []string{"main"}, Architectures: []string{"amd64"}},
		}},
	}
	r, err := NewRunner(cfg, storage.NewMemory(), observability.NewMetrics("test"), slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUnsupportedRepoIsListedButNotRunnable(t *testing.T) {
	r := testRunner(t)
	st := r.Status()
	if len(st) != 1 || st[0].Unsupported == "" {
		t.Fatalf("status = %+v", st)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if err := r.Trigger("ubuntu"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Trigger err = %v, want ErrUnsupported", err)
	}
	if _, err := r.RunOnce(context.Background(), "ubuntu"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("RunOnce err = %v, want ErrUnsupported", err)
	}
	if err := r.Trigger("nope"); !errors.Is(err, ErrUnknownRepo) {
		t.Fatalf("err = %v, want ErrUnknownRepo", err)
	}
}
