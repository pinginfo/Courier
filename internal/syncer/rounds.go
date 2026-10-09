package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"courier/internal/fetcher"
	"courier/internal/provider"
)

// failKind says what to do about a package that failed in a round.
type failKind int

const (
	// failTransient: network trouble, 5xx, S3 errors, or a download that did not
	// verify. Worth retrying after a pause.
	failTransient failKind = iota
	// failNotFound: the package is gone upstream. The metadata is probably stale,
	// so retry after fetching it again.
	failNotFound
	// failPermanent: retrying cannot help (403 and other 4xx, local errors).
	failPermanent
)

type failure struct {
	pkg  provider.Package
	kind failKind
	err  error
}

func classifyFailure(err error) failKind {
	if c, ok := fetcher.Classify(err); ok {
		switch c {
		case fetcher.NotFound:
			return failNotFound
		case fetcher.Permanent:
			return failPermanent
		}
	}
	return failTransient
}

// confirmedSet holds the paths verified to be in the bucket during this run.
type confirmedSet struct {
	mu sync.Mutex
	m  map[string]bool
}

func newConfirmedSet() *confirmedSet { return &confirmedSet{m: map[string]bool{}} }

func (c *confirmedSet) add(path string) {
	c.mu.Lock()
	c.m[path] = true
	c.mu.Unlock()
}

func (c *confirmedSet) has(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[path]
}

// missing returns the packages not yet confirmed, in their original order.
func (c *confirmedSet) missing(pkgs []provider.Package) []provider.Package {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]provider.Package, 0, len(pkgs))
	for _, p := range pkgs {
		if !c.m[p.Path] {
			out = append(out, p)
		}
	}
	return out
}

func cleanupPlan(p *provider.Plan) {
	if p != nil && p.Cleanup != nil {
		p.Cleanup()
	}
}

func vanishedPaths(failures []failure) map[string]bool {
	out := map[string]bool{}
	for _, f := range failures {
		if f.kind == failNotFound {
			out[f.pkg.Path] = true
		}
	}
	return out
}

func stillListed(plan *provider.Plan, paths map[string]bool) int {
	n := 0
	for _, p := range plan.Packages {
		if paths[p.Path] {
			n++
		}
	}
	return n
}

// roundPause grows with the round: base, 2*base, 3*base ... capped at 5 minutes.
func roundPause(base time.Duration, round int) time.Duration {
	return min(base*time.Duration(round), 5*time.Minute)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// planWithRetry asks the provider for a plan. A plan that fails because of a
// transient network problem is retried after a pause; verification failures
// (bad signature, wrong checksum) and other permanent errors are not.
func (e *Engine) planWithRetry(ctx context.Context, log *slog.Logger) (*provider.Plan, error) {
	maxRounds := e.defaults.RetryRoundCount()
	for attempt := 0; ; attempt++ {
		plan, err := e.prov.Plan(ctx)
		if err == nil {
			return plan, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("plan: %w", err)
		}
		class, fromFetcher := fetcher.Classify(err)
		if !fromFetcher || class != fetcher.Transient || attempt >= maxRounds {
			e.metrics.Errors.WithLabelValues(e.repo.Name, "plan", "provider").Inc()
			return nil, fmt.Errorf("plan: %w", err)
		}
		pause := roundPause(e.retryPause, attempt+1)
		log.Warn("planning failed with a network error, will retry", "attempt", attempt+1, "of", maxRounds, "pause", pause, "error", err)
		if err := sleepCtx(ctx, pause); err != nil {
			return nil, fmt.Errorf("plan: %w", err)
		}
	}
}

// beforeRetry decides what happens after a round with failures. It returns an
// error when the run should give up, or after the pause whether the metadata
// must be fetched again (some packages vanished upstream).
func (e *Engine) beforeRetry(ctx context.Context, log *slog.Logger, round, maxRounds int, failures []failure, res *Result) (replan bool, err error) {
	var notFound int
	var permanent *failure
	for i := range failures {
		switch failures[i].kind {
		case failNotFound:
			notFound++
		case failPermanent:
			if permanent == nil {
				permanent = &failures[i]
			}
		}
	}
	if permanent != nil {
		return false, fmt.Errorf("package %s failed permanently (retrying cannot help); metadata not published: %w",
			permanent.pkg.Path, permanent.err)
	}
	first := failures[0]
	if round >= maxRounds {
		return false, fmt.Errorf("%d package(s) still failing after %d retry round(s); metadata not published; first: %s: %w",
			len(failures), maxRounds, first.pkg.Path, first.err)
	}
	pause := roundPause(e.retryPause, round+1)
	res.RetryRounds++
	e.metrics.RetryRounds.WithLabelValues(e.repo.Name).Inc()
	log.Warn("some packages failed, retrying after a pause", "retry_round", round+1, "of", maxRounds, "failed", len(failures),
		"vanished_upstream", notFound, "pause", pause, "example", first.pkg.Path, "error", first.err)
	if err := sleepCtx(ctx, pause); err != nil {
		return false, err
	}
	return notFound > 0, nil
}

// transfer downloads and uploads the given packages (those not yet confirmed in
// the bucket) and records each success in confirmed. Failures are returned,
// not turned into an error; an error means the context was cancelled.
func (e *Engine) transfer(ctx context.Context, log *slog.Logger, todo []provider.Package, confirmed *confirmedSet, res *Result) ([]failure, error) {
	name := e.repo.Name
	// Every package is checked against the bucket, never against the manifest:
	// the manifest says what we meant to upload, the bucket says what is there.
	e.metrics.Files.WithLabelValues(name, "pending").Set(float64(len(todo)))
	log.Info("transfer starting", "packages", len(todo))

	var downloaded, present, bytes atomic.Int64
	var mu sync.Mutex
	var failures []failure
	budget, sem := e.disk.total, e.disk.sem
	log.Info("disk budget for in-flight downloads (shared by all repos)", "bytes", budget, "concurrency", e.defaults.Concurrency)

	done := make(chan struct{})
	var progress sync.WaitGroup
	progress.Add(1)
	go func() {
		defer progress.Done()
		tick := time.NewTicker(e.progressEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				mu.Lock()
				nFailed := len(failures)
				mu.Unlock()
				fin := downloaded.Load() + present.Load() + int64(nFailed)
				e.metrics.Files.WithLabelValues(name, "pending").Set(float64(int64(len(todo)) - fin))
				log.Info("transfer progress", "done", fin, "of", len(todo), "downloaded", downloaded.Load(),
					"already_present", present.Load(), "failed", nFailed, "bytes", bytes.Load())
			}
		}
	}()

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, e.defaults.Concurrency))
	for _, p := range todo {
		g.Go(func() error {
			n, did, err := e.mirrorOne(gctx, p, sem, budget)
			switch {
			case err != nil:
				if ctx.Err() != nil {
					return ctx.Err()
				}
				kind := classifyFailure(err)
				e.metrics.Errors.WithLabelValues(name, "transfer", errorReason(err)).Inc()
				log.Warn("package failed", "path", p.Path, "kind", kind.String(), "error", err)
				mu.Lock()
				failures = append(failures, failure{pkg: p, kind: kind, err: err})
				mu.Unlock()
			case did:
				confirmed.add(p.Path)
				downloaded.Add(1)
				bytes.Add(n)
				// Count as we go, so throughput is visible while a long sync runs.
				e.metrics.Bytes.WithLabelValues(name, "downloaded").Add(float64(n))
				e.metrics.Bytes.WithLabelValues(name, "uploaded").Add(float64(n))
			default:
				confirmed.add(p.Path)
				present.Add(1)
			}
			return nil
		})
	}
	_ = g.Wait()
	close(done)
	progress.Wait()
	res.Downloaded += int(downloaded.Load())
	res.Skipped += int(present.Load())
	res.Bytes += bytes.Load()
	e.metrics.Files.WithLabelValues(name, "failed").Set(float64(len(failures)))
	e.metrics.Files.WithLabelValues(name, "pending").Set(0)

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return failures, nil
}

func (k failKind) String() string {
	switch k {
	case failNotFound:
		return "not_found"
	case failPermanent:
		return "permanent"
	default:
		return "transient"
	}
}
