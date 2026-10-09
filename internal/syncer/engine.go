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
	"time"

	"golang.org/x/sync/semaphore"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/observability"
	"courier/internal/provider"
	"courier/internal/state"
	"courier/internal/storage"
)

// unknownSizeWeight is the disk reserved for a package whose size is not known
// in advance.
const unknownSizeWeight = 64 << 20

// ErrBusy is returned when a sync of the same repo is already running.
var ErrBusy = errors.New("sync already running")

// Result summarises one sync run.
type Result struct {
	RunID       string        `json:"run_id"`
	Started     time.Time     `json:"started"`
	Duration    time.Duration `json:"duration"`
	Planned     int           `json:"planned"`
	Skipped     int           `json:"skipped"`
	Downloaded  int           `json:"downloaded"`
	Bytes       int64         `json:"bytes"`
	Failed      int           `json:"failed"`
	Published   int           `json:"published"`
	Deleted     int           `json:"deleted"`
	RetryRounds int           `json:"retry_rounds"`
	Replans     int           `json:"replans"`
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

	// progressEvery is how often transfer progress is logged.
	progressEvery time.Duration

	// retryPause is the base pause between retry rounds (grows with the round).
	retryPause time.Duration

	// disk bounds the temporary files of in-flight downloads (shared, see UseDiskBudget).
	disk *DiskBudget

	mu sync.Mutex
}

// NewEngine builds an engine for repo using prov.
func NewEngine(repo config.Repo, defaults config.Defaults, prov provider.Provider, f *fetcher.Fetcher,
	store storage.Storage, m *observability.Metrics, log *slog.Logger) *Engine {
	return &Engine{repo: repo, defaults: defaults, prov: prov, fetch: f, store: store, metrics: m, log: log, now: time.Now,
		progressEvery: 30 * time.Second, retryPause: 30 * time.Second,
		disk: NewDiskBudget(defaults.MaxTempBytes())}
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
			if ctx.Err() != nil {
				// Interrupted (shutdown or a cancelled request), not a failure: nothing
				// was published, and the next run resumes where this one stopped.
				e.metrics.SyncRuns.WithLabelValues(name, "cancelled").Inc()
				log.Info("sync cancelled", "reason", context.Cause(ctx), "duration", res.Duration.Round(time.Millisecond),
					"downloaded", res.Downloaded, "bytes", res.Bytes)
				return
			}
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
	plan, err := e.planWithRetry(ctx, log)
	if err != nil {
		return res, err
	}
	defer func() { cleanupPlan(plan) }()
	res.Planned = len(plan.Packages)
	log.Info("plan ready", "packages", len(plan.Packages), "metadata", len(plan.Metadata))

	old, err := state.Load(ctx, e.store, name)
	if err != nil {
		e.metrics.Errors.WithLabelValues(name, "state", "load").Inc()
		return res, err
	}

	// Rounds: transfer what is not yet confirmed in the bucket; if packages still
	// fail, pause and go again (re-planning first if any of them vanished
	// upstream), up to RetryRounds times. Nothing is published until every package
	// of the final plan is confirmed.
	confirmed := newConfirmedSet()
	maxRounds := e.defaults.RetryRoundCount()
	for round := 0; ; round++ {
		failures, err := e.transfer(ctx, log.With("round", round+1), confirmed.missing(plan.Packages), confirmed, &res)
		if err != nil {
			return res, err
		}
		res.Failed = len(failures)
		if len(failures) == 0 {
			break
		}
		replan, err := e.beforeRetry(ctx, log, round, maxRounds, failures, &res)
		if err != nil {
			return res, err
		}
		if replan {
			vanished := vanishedPaths(failures)
			fresh, err := e.planWithRetry(ctx, log)
			if err != nil {
				return res, fmt.Errorf("re-plan: %w", err)
			}
			cleanupPlan(plan)
			plan = fresh
			res.Planned = len(plan.Packages)
			res.Replans++
			e.metrics.Replans.WithLabelValues(name).Inc()
			log.Info("re-planned after packages vanished upstream", "packages", len(plan.Packages),
				"vanished", len(vanished), "still_listed", stillListed(plan, vanished))
		}
	}
	res.Failed = 0

	// Last line of defence: never publish an index that refers to a package this
	// run has not confirmed in the bucket.
	for _, p := range plan.Packages {
		if !confirmed.has(p.Path) {
			e.metrics.Errors.WithLabelValues(name, "publish", "unconfirmed").Inc()
			return res, fmt.Errorf("refusing to publish: %s is not confirmed in the bucket", p.Path)
		}
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

// mirrorOne makes sure one package exists in the bucket. did reports whether it
// was downloaded (false when it was already present).
func (e *Engine) mirrorOne(ctx context.Context, p provider.Package, sem *semaphore.Weighted, budget int64) (n int64, did bool, err error) {
	key := e.key(p.Path)
	if info, err := e.store.Stat(ctx, key); err == nil && (p.Size == 0 || info.Size == p.Size) {
		return 0, false, nil // already in the bucket (also adopts uploads left by an interrupted run)
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return 0, false, fmt.Errorf("stat %s: %w", key, err)
	}

	// Reserve disk for the temporary file. A package bigger than the whole budget
	// is let through on its own rather than never.
	weight := p.Size
	if weight <= 0 {
		weight = unknownSizeWeight
	}
	weight = min(weight, budget)
	if err := sem.Acquire(ctx, weight); err != nil {
		return 0, false, err
	}
	defer sem.Release(weight)

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
	// Do not trust a successful PUT: confirm the object is really there with the
	// right size before this package counts as mirrored.
	info, err := e.store.Stat(ctx, key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return 0, false, fmt.Errorf("upload: %s was accepted but is not in the bucket", key)
	case err != nil:
		return 0, false, fmt.Errorf("upload: confirm %s: %w", key, err)
	case info.Size != n:
		return 0, false, fmt.Errorf("upload: %s has %d bytes in the bucket, expected %d", key, info.Size, n)
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
		if err := e.putMetadata(ctx, m, ct); err != nil {
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

// putMetadata uploads one metadata file from memory or from its staged local
// file, then confirms it is in the bucket with the right size.
func (e *Engine) putMetadata(ctx context.Context, m provider.Metadata, contentType string) error {
	key := e.key(m.Path)
	var (
		r    io.Reader = bytes.NewReader(m.Body)
		size           = int64(len(m.Body))
	)
	if m.File != "" {
		f, err := os.Open(m.File) // #nosec G304 G703 -- staged by the provider in its own temp dir
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		r, size = f, st.Size()
	}
	if err := e.store.Put(ctx, key, r, size, contentType); err != nil {
		return err
	}
	info, err := e.store.Stat(ctx, key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return fmt.Errorf("%s was accepted but is not in the bucket", key)
	case err != nil:
		return err
	case info.Size != size:
		return fmt.Errorf("%s has %d bytes in the bucket, expected %d", key, info.Size, size)
	}
	return nil
}

// DiskBudget limits the temporary disk used by in-flight package downloads. One
// budget is shared by every engine in the process, so syncing several repos at
// the same time cannot add up to more than the configured total.
type DiskBudget struct {
	sem   *semaphore.Weighted
	total int64
}

// NewDiskBudget returns a budget of total bytes.
func NewDiskBudget(total int64) *DiskBudget {
	return &DiskBudget{sem: semaphore.NewWeighted(total), total: total}
}

// UseDiskBudget makes the engine draw from a shared budget instead of its own.
func (e *Engine) UseDiskBudget(b *DiskBudget) { e.disk = b }
