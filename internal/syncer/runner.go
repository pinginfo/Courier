package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/observability"
	"courier/internal/provider"
	"courier/internal/provider/apk"
	"courier/internal/provider/apt"
	"courier/internal/storage"
)

// ErrUnknownRepo is returned for a repo name that is not configured.
var ErrUnknownRepo = errors.New("unknown repo")

// ErrUnsupported is returned for a repo whose type has no provider yet.
var ErrUnsupported = errors.New("repo type not implemented yet")

// RepoStatus is the externally visible state of one repo.
type RepoStatus struct {
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Schedule    string     `json:"schedule"`
	Running     bool       `json:"running"`
	Unsupported string     `json:"unsupported,omitempty"`
	LastRun     *time.Time `json:"last_run,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastResult  string     `json:"last_result,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastRunInfo *Result    `json:"last_run_info,omitempty"`
}

type entry struct {
	repo   config.Repo
	engine *Engine // nil when the repo type is not implemented yet
	status RepoStatus
}

// Runner owns one Engine per repo, schedules them and tracks their status.
type Runner struct {
	log     *slog.Logger
	entries map[string]*entry
	order   []string
	cron    *cron.Cron

	mu  sync.Mutex
	wg  sync.WaitGroup
	ctx context.Context
}

// NewProvider builds the provider for a repo, or returns an error if the repo
// type is not implemented yet.
func NewProvider(repo config.Repo, f *fetcher.Fetcher, log *slog.Logger) (provider.Provider, error) {
	switch repo.Type {
	case config.TypeAPK:
		return apk.New(repo, f, log)
	case config.TypeAPT:
		return apt.New(repo, f, log)
	default:
		return nil, fmt.Errorf("%w: type %q", ErrUnsupported, repo.Type)
	}
}

// NewRunner builds engines for every repo in cfg. Repos whose type is not
// implemented yet are listed as unsupported and never scheduled. A provider
// that is implemented but misconfigured (for example missing keys) is an error.
func NewRunner(cfg *config.Config, store storage.Storage, m *observability.Metrics, log *slog.Logger, userAgent string) (*Runner, error) {
	bps, err := config.ParseRate(cfg.Defaults.RateLimit)
	if err != nil {
		return nil, err
	}
	r := &Runner{log: log, entries: map[string]*entry{}, cron: cron.New()}
	for _, repo := range cfg.Repos {
		name := repo.Name
		e := &entry{repo: repo, status: RepoStatus{Name: name, Type: repo.Type, Schedule: repo.Schedule}}
		f := fetcher.New(fetcher.Options{
			UserAgent: userAgent, BytesPerSec: bps,
			Observe: func(status string, d time.Duration) {
				m.UpstreamDuration.WithLabelValues(name, status).Observe(d.Seconds())
			},
		})
		prov, err := NewProvider(repo, f, log)
		switch {
		case err == nil:
			e.engine = NewEngine(repo, cfg.Defaults, prov, f, store, m, log)
		case !errors.Is(err, ErrUnsupported):
			return nil, fmt.Errorf("repo %s: %w", name, err)
		default:
			e.status.Unsupported = err.Error()
			log.Warn("repo will not be synced", "repo", name, "reason", err)
		}
		r.entries[name] = e
		r.order = append(r.order, name)
	}
	return r, nil
}

// Start schedules every supported repo. Syncs run until ctx is cancelled.
func (r *Runner) Start(ctx context.Context) error {
	r.ctx = ctx
	for _, name := range r.order {
		e := r.entries[name]
		if e.engine == nil {
			continue
		}
		if _, err := r.cron.AddFunc(e.repo.Schedule, func() { _ = r.Trigger(name) }); err != nil {
			return fmt.Errorf("schedule %s: %w", name, err)
		}
		r.log.Info("scheduled", "repo", name, "schedule", e.repo.Schedule)
	}
	r.cron.Start()
	return nil
}

// Stop stops scheduling and waits for running syncs to finish (they stop early
// once the context given to Start is cancelled).
func (r *Runner) Stop() {
	<-r.cron.Stop().Done()
	r.wg.Wait()
}

// Trigger starts a sync of name in the background. It returns ErrBusy if one is
// already running and ErrUnknownRepo if name is not configured.
func (r *Runner) Trigger(name string) error {
	e, ok := r.entries[name]
	if !ok {
		return ErrUnknownRepo
	}
	if e.engine == nil {
		return fmt.Errorf("repo %s: %w (%s)", name, ErrUnsupported, e.status.Unsupported)
	}
	if r.ctx == nil {
		return errors.New("runner not started")
	}
	r.mu.Lock()
	if e.status.Running {
		r.mu.Unlock()
		return ErrBusy
	}
	e.status.Running = true
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		_, _ = r.run(r.ctx, e)
	}()
	return nil
}

// RunOnce runs a sync of name in the foreground (used by `courier sync`).
func (r *Runner) RunOnce(ctx context.Context, name string) (Result, error) {
	e, ok := r.entries[name]
	if !ok {
		return Result{}, ErrUnknownRepo
	}
	if e.engine == nil {
		return Result{}, fmt.Errorf("repo %s: %w (%s)", name, ErrUnsupported, e.status.Unsupported)
	}
	return r.run(ctx, e)
}

func (r *Runner) run(ctx context.Context, e *entry) (Result, error) {
	r.mu.Lock()
	e.status.Running = true
	r.mu.Unlock()

	res, err := e.engine.Run(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	e.status.Running = false
	now := time.Now()
	e.status.LastRun = &now
	e.status.LastRunInfo = &res
	if err != nil {
		e.status.LastResult, e.status.LastError = "failure", err.Error()
	} else {
		e.status.LastResult, e.status.LastError = "success", ""
		e.status.LastSuccess = &now
	}
	return res, err
}

// Names lists configured repo names in config order.
func (r *Runner) Names() []string { return append([]string(nil), r.order...) }

// Status returns a snapshot of every repo.
func (r *Runner) Status() []RepoStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RepoStatus, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.entries[name].status)
	}
	return out
}
