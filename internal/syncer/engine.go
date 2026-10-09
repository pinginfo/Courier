// Package syncer runs repository syncs: plan, diff, download, upload, publish
// metadata, garbage-collect.
package syncer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/observability"
	"courier/internal/provider"
	"courier/internal/state"
	"courier/internal/storage"
)

// ErrBusy is returned when a sync of the same repo is already running.
var ErrBusy = errors.New("sync already running")

// Result summarises one sync run.
type Result struct {
	RunID      string        `json:"run_id"`
	Started    time.Time     `json:"started"`
	Duration   time.Duration `json:"duration"`
	Planned    int           `json:"planned"`
	Skipped    int           `json:"skipped"`
	Downloaded int           `json:"downloaded"`
	Bytes      int64         `json:"bytes"`
	Failed     int           `json:"failed"`
	Published  int           `json:"published"`
	Deleted    int           `json:"deleted"`
}

// Engine syncs one repository.
type Engine struct {
	repo     config.Repo
	defaults config.Defaults
	prov     provider.Provider
	fetch    *fetcher.Fetcher
	store    storage.Storage
	metrics  *observability.Metrics
	log      *slog.Logger
	now      func() time.Time

	mu sync.Mutex
}

// NewEngine builds an engine for repo using prov.
func NewEngine(repo config.Repo, defaults config.Defaults, prov provider.Provider, f *fetcher.Fetcher,
	store storage.Storage, m *observability.Metrics, log *slog.Logger) *Engine {
	return &Engine{repo: repo, defaults: defaults, prov: prov, fetch: f, store: store, metrics: m, log: log, now: time.Now}
}

func newRunID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *Engine) key(path string) string { return e.repo.Prefix + "/" + path }

func (e *Engine) url(path string) string { return strings.TrimRight(e.repo.Upstream, "/") + "/" + path }

// Run performs one sync. Concurrent runs of the same repo return ErrBusy.
func (e *Engine) Run(ctx context.Context) (res Result, err error) {
	if !e.mu.TryLock() {
		return Result{}, ErrBusy
	}
	defer e.mu.Unlock()

	res = Result{RunID: newRunID(), Started: e.now()}
	log := e.log.With("repo", e.repo.Name, "run_id", res.RunID)
	name := e.repo.Name
	e.metrics.SyncInProgress.WithLabelValues(name).Set(1)
	log.Info("sync started")
	defer func() {
		res.Duration = e.now().Sub(res.Started)
		e.metrics.SyncInProgress.WithLabelValues(name).Set(0)
		e.metrics.SyncDuration.WithLabelValues(name).Observe(res.Duration.Seconds())
		if err != nil {
			e.metrics.SyncRuns.WithLabelValues(name, "failure").Inc()
			log.Error("sync failed", "error", err, "duration", res.Duration.Round(time.Millisecond))
			return
		}
		e.metrics.SyncRuns.WithLabelValues(name, "success").Inc()
		e.metrics.SyncLastSuccess.WithLabelValues(name).SetToCurrentTime()
		log.Info("sync finished", "duration", res.Duration.Round(time.Millisecond),
			"planned", res.Planned, "skipped", res.Skipped, "downloaded", res.Downloaded,
			"bytes", res.Bytes, "published", res.Published, "deleted", res.Deleted)
	}()

	log.Info("planning")
	plan, err := e.prov.Plan(ctx)
	if err != nil {
		e.metrics.Errors.WithLabelValues(name, "plan", "provider").Inc()
		return res, fmt.Errorf("plan: %w", err)
	}
	res.Planned = len(plan.Packages)
	log.Info("plan ready", "packages", len(plan.Packages), "metadata", len(plan.Metadata))

	old, err := state.Load(ctx, e.store, name)
	if err != nil {
		e.metrics.Errors.WithLabelValues(name, "state", "load").Inc()
		return res, err
	}

	if err := e.transfer(ctx, log, plan, old, &res); err != nil {
		return res, err
	}

	if err := e.publish(ctx, log, plan, &res); err != nil {
		return res, err
	}

	next := e.nextManifest(plan, old)
	res.Deleted = e.collect(ctx, log, next)
	if err := state.Save(ctx, e.store, name, next); err != nil {
		e.metrics.Errors.WithLabelValues(name, "state", "save").Inc()
		return res, fmt.Errorf("save manifest: %w", err)
	}
	e.metrics.Files.WithLabelValues(name, "mirrored").Set(float64(len(next.Packages)))
	e.metrics.Files.WithLabelValues(name, "pending").Set(0)
	e.metrics.Files.WithLabelValues(name, "failed").Set(0)
	return res, nil
}

// transfer downloads and uploads every package that is not already mirrored.
// It returns an error if any package failed; metadata must not be published then.
func (e *Engine) transfer(ctx context.Context, log *slog.Logger, plan *provider.Plan, old *state.Manifest, res *Result) error {
	name := e.repo.Name
	var todo []provider.Package
	for _, p := range plan.Packages {
		if size, ok := old.Packages[p.Path]; ok && (p.Size == 0 || size == p.Size) {
			res.Skipped++
			continue
		}
		todo = append(todo, p)
	}
	e.metrics.Files.WithLabelValues(name, "pending").Set(float64(len(todo)))
	log.Info("transfer starting", "to_check", len(todo), "already_mirrored", res.Skipped)

	var downloaded, skipped, failed, bytes atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, e.defaults.Concurrency))
	for _, p := range todo {
		g.Go(func() error {
			n, did, err := e.mirrorOne(gctx, p)
			switch {
			case err != nil:
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failed.Add(1)
				e.metrics.Errors.WithLabelValues(name, "transfer", errorReason(err)).Inc()
				log.Warn("package failed", "path", p.Path, "error", err)
			case did:
				downloaded.Add(1)
				bytes.Add(n)
			default:
				skipped.Add(1)
			}
			return nil
		})
	}
	_ = g.Wait()
	res.Downloaded, res.Failed, res.Bytes = int(downloaded.Load()), int(failed.Load()), bytes.Load()
	res.Skipped += int(skipped.Load())
	e.metrics.Bytes.WithLabelValues(name, "downloaded").Add(float64(res.Bytes))
	e.metrics.Bytes.WithLabelValues(name, "uploaded").Add(float64(res.Bytes))
	e.metrics.Files.WithLabelValues(name, "failed").Set(float64(res.Failed))
	e.metrics.Files.WithLabelValues(name, "pending").Set(0)

	if err := ctx.Err(); err != nil {
		return err
	}
	if res.Failed > 0 {
		return fmt.Errorf("%d of %d packages failed; metadata not published", res.Failed, len(todo))
	}
	return nil
}

// mirrorOne makes sure one package exists in the bucket. did reports whether it
// was downloaded (false when it was already present).
func (e *Engine) mirrorOne(ctx context.Context, p provider.Package) (n int64, did bool, err error) {
	key := e.key(p.Path)
	if info, err := e.store.Stat(ctx, key); err == nil && (p.Size == 0 || info.Size == p.Size) {
		return 0, false, nil // adopt an object left by an interrupted run
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return 0, false, fmt.Errorf("stat %s: %w", key, err)
	}

	tmp, err := os.CreateTemp("", "courier-*")
	if err != nil {
		return 0, false, err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	n, err = e.fetch.File(ctx, e.url(p.Path), tmp)
	if err != nil {
		return 0, false, fmt.Errorf("download: %w", err)
	}
	if p.Size > 0 && n != p.Size {
		return 0, false, fmt.Errorf("size mismatch for %s: got %d, want %d", p.Path, n, p.Size)
	}
	if p.Verify != nil {
		if err := p.Verify(tmp.Name()); err != nil {
			return 0, false, fmt.Errorf("verify %s: %w", p.Path, err)
		}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, false, err
	}
	if err := e.store.Put(ctx, key, tmp, n, "application/octet-stream"); err != nil {
		return 0, false, fmt.Errorf("upload: %w", err)
	}
	return n, true, nil
}

func errorReason(err error) string {
	switch {
	case strings.Contains(err.Error(), "size mismatch"):
		return "size"
	case strings.Contains(err.Error(), "verify "):
		return "verify"
	case strings.Contains(err.Error(), "upload:"):
		return "upload"
	case strings.Contains(err.Error(), "download:"):
		return "download"
	default:
		return "other"
	}
}

// publish uploads the metadata files in the order the provider gave them.
func (e *Engine) publish(ctx context.Context, log *slog.Logger, plan *provider.Plan, res *Result) error {
	for _, m := range plan.Metadata {
		ct := m.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		if err := e.store.Put(ctx, e.key(m.Path), bytes.NewReader(m.Body), int64(len(m.Body)), ct); err != nil {
			e.metrics.Errors.WithLabelValues(e.repo.Name, "publish", "upload").Inc()
			return fmt.Errorf("publish %s: %w", m.Path, err)
		}
		res.Published++
	}
	log.Info("metadata published", "files", res.Published)
	return nil
}

// nextManifest builds the manifest after a successful run: everything in plan is
// owned and live; whatever dropped out of the plan becomes an orphan (kept until
// the grace period passes).
func (e *Engine) nextManifest(plan *provider.Plan, old *state.Manifest) *state.Manifest {
	next := state.New()
	next.UpdatedAt = e.now()
	live := map[string]bool{}
	for _, p := range plan.Packages {
		size := p.Size
		if size == 0 {
			size = old.Packages[p.Path]
		}
		next.Packages[p.Path] = size
		live[p.Path] = true
	}
	for _, m := range plan.Metadata {
		next.Metadata = append(next.Metadata, m.Path)
		live[m.Path] = true
	}
	// Orphans that came back to life are no longer orphans.
	for path, o := range old.Orphans {
		if !live[path] {
			next.Orphans[path] = o
		}
	}
	// Newly dropped files start their grace period now.
	dropped := func(path string) {
		if _, ok := next.Orphans[path]; !live[path] && !ok {
			next.Orphans[path] = state.Orphan{Since: e.now()}
		}
	}
	for path := range old.Packages {
		dropped(path)
	}
	for _, path := range old.Metadata {
		dropped(path)
	}
	return next
}

// collect deletes orphans older than the grace period and removes them from next.
func (e *Engine) collect(ctx context.Context, log *slog.Logger, next *state.Manifest) int {
	deleted := 0
	for path, o := range next.Orphans {
		if e.now().Sub(o.Since) < e.defaults.Retention.GracePeriod {
			continue
		}
		if err := e.store.Delete(ctx, e.key(path)); err != nil {
			e.metrics.Errors.WithLabelValues(e.repo.Name, "gc", "delete").Inc()
			log.Warn("gc delete failed", "path", path, "error", err)
			continue
		}
		delete(next.Orphans, path)
		deleted++
	}
	e.metrics.GCDeletedObjects.WithLabelValues(e.repo.Name).Add(float64(deleted))
	if deleted > 0 {
		log.Info("gc finished", "deleted", deleted, "grace_pending", len(next.Orphans))
	}
	return deleted
}
