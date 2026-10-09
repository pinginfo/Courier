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
			Name: "fedora", Type: "npm", Upstream: "https://example.org", Prefix: "fedora", Schedule: "0 4 * * *",
		}},
	}
	r, err := NewRunner(cfg, storage.NewMemory(), observability.NewMetrics("test"), slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A type with no provider (a future ecosystem) is listed but never scheduled.
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
	if err := r.Trigger("fedora"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Trigger err = %v, want ErrUnsupported", err)
	}
	if _, err := r.RunOnce(context.Background(), "fedora"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("RunOnce err = %v, want ErrUnsupported", err)
	}
	if err := r.Trigger("nope"); !errors.Is(err, ErrUnknownRepo) {
		t.Fatalf("err = %v, want ErrUnknownRepo", err)
	}
}

func TestRunnerSharesOneDiskBudgetAcrossRepos(t *testing.T) {
	_, keys := apkKeys(t)
	repo := func(name string) config.Repo {
		return config.Repo{
			Name: name, Type: config.TypeAPK, Upstream: "https://example.org", Prefix: name, Schedule: "0 4 * * *",
			APK: &config.APK{Branches: []string{"v3.20"}, Repos: []string{"main"}, Architectures: []string{"x86_64"}, KeysDir: keys},
		}
	}
	cfg := &config.Config{Defaults: config.Defaults{Concurrency: 2, MaxTempSize: "1GB"}, Repos: []config.Repo{repo("a"), repo("b"), repo("c")}}
	r, err := NewRunner(cfg, storage.NewMemory(), observability.NewMetrics("test"), slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	first := r.entries["a"].engine.disk
	for _, name := range []string{"b", "c"} {
		if r.entries[name].engine.disk != first {
			t.Fatalf("repo %s has its own disk budget; it must share one with the others", name)
		}
	}
	if first.total != 1<<30 {
		t.Fatalf("budget = %d, want 1 GiB", first.total)
	}
}
