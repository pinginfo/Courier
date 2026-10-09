package syncer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"courier/internal/provider/apk/apktest"
)

// concurrencyProbe wraps the upstream and records how many package downloads
// are in flight at once.
type concurrencyProbe struct {
	cur, max atomic.Int32
	hold     time.Duration
}

func (c *concurrencyProbe) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".apk") {
			n := c.cur.Add(1)
			for {
				m := c.max.Load()
				if n <= m || c.max.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(c.hold)
			defer c.cur.Add(-1)
		}
		next.ServeHTTP(w, r)
	})
}

func manyPackages(e *env, n int) []apktest.Pkg {
	var pkgs []apktest.Pkg
	for i := 0; i < n; i++ {
		pkgs = append(pkgs, apktest.BuildPkg(e.key, fmt.Sprintf("pkg%d", i), "1.0-r0", strings.Repeat("x", 4000)))
	}
	return pkgs
}

func TestDiskBudgetBoundsConcurrentDownloads(t *testing.T) {
	probe := &concurrencyProbe{hold: 40 * time.Millisecond}
	e := newEnv(t, probe.wrap)
	pkgs := manyPackages(e, 8)
	e.publishUpstream(pkgs...)

	e.eng.defaults.Concurrency = 6
	size := len(pkgs[0].Bytes)
	// Room for exactly two packages at a time.
	e.eng.UseDiskBudget(NewDiskBudget(int64(2*size + 50)))

	res := e.mustRun()
	if res.Downloaded != 8 {
		t.Fatalf("result = %+v", res)
	}
	if got := probe.max.Load(); got > 2 {
		t.Fatalf("%d downloads were in flight at once; the disk budget allows 2", got)
	}
	if got := probe.max.Load(); got < 2 {
		t.Fatalf("only %d download at a time; the budget should still allow 2", got)
	}
}

func TestPackageLargerThanBudgetStillDownloadsAlone(t *testing.T) {
	probe := &concurrencyProbe{hold: 20 * time.Millisecond}
	e := newEnv(t, probe.wrap)
	e.publishUpstream(manyPackages(e, 4)...)
	e.eng.defaults.Concurrency = 4
	e.eng.UseDiskBudget(NewDiskBudget(1)) // smaller than every package

	done := make(chan Result, 1)
	go func() { done <- e.mustRun() }()
	select {
	case res := <-done:
		if res.Downloaded != 4 {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sync deadlocked on an oversized package")
	}
	if got := probe.max.Load(); got != 1 {
		t.Fatalf("oversized packages must download one at a time, saw %d", got)
	}
}

func TestCancelledRunIsNotCountedAsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := newEnv(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ".apk") {
				cancel()
			}
			next.ServeHTTP(w, r)
		})
	})
	e.publishUpstream(apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa"))
	if _, err := e.eng.Run(ctx); err == nil {
		t.Fatal("expected an error after cancel")
	}
	m := e.eng.metrics
	if got := testutil.ToFloat64(m.SyncRuns.WithLabelValues("alpine", "cancelled")); got != 1 {
		t.Errorf("cancelled runs = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.SyncRuns.WithLabelValues("alpine", "failure")); got != 0 {
		t.Errorf("failures = %v, want 0 (a cancelled run is not a failure)", got)
	}
	if got := testutil.ToFloat64(m.Errors.WithLabelValues("alpine", "transfer", "download")); got != 0 {
		t.Errorf("transfer errors = %v, want 0", got)
	}
}

func TestBytesAreCountedWhileTheSyncIsStillRunning(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	e := newEnv(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "slow-1.0-r0.apk") {
				<-release // the last package stays in flight
			}
			next.ServeHTTP(w, r)
		})
	})
	fast := apktest.BuildPkg(e.key, "fast", "1.0-r0", strings.Repeat("a", 3000))
	slow := apktest.BuildPkg(e.key, "slow", "1.0-r0", "b")
	e.publishUpstream(fast, slow)

	done := make(chan error, 1)
	go func() { _, err := e.run(); done <- err }()

	deadline := time.After(10 * time.Second)
	for testutil.ToFloat64(e.eng.metrics.Bytes.WithLabelValues("alpine", "downloaded")) == 0 {
		select {
		case <-deadline:
			once.Do(func() { close(release) })
			t.Fatal("bytes not counted while the sync was running")
		case <-time.After(10 * time.Millisecond):
		}
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProgressIsLoggedDuringTransfer(t *testing.T) {
	var buf strings.Builder
	probe := &concurrencyProbe{hold: 60 * time.Millisecond}
	e := newEnv(t, probe.wrap)
	e.publishUpstream(manyPackages(e, 3)...)
	e.eng.progressEvery = 20 * time.Millisecond
	e.eng.defaults.Concurrency = 1
	e.eng.log = slogTo(&buf)
	e.mustRun()
	if !strings.Contains(buf.String(), "transfer progress") {
		t.Fatalf("no progress line logged:\n%s", buf.String())
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func slogTo(b *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Writer(&syncWriter{w: b}), nil))
}

// Two repos syncing at the same moment must share one disk budget.
func TestDiskBudgetIsSharedAcrossRepos(t *testing.T) {
	probe := &concurrencyProbe{hold: 60 * time.Millisecond}
	a := newEnv(t, probe.wrap)
	b := newEnv(t, probe.wrap)
	a.publishUpstream(manyPackages(a, 6)...)
	b.publishUpstream(manyPackages(b, 6)...)

	size := len(manyPackages(a, 1)[0].Bytes)
	shared := NewDiskBudget(int64(2*size + 50)) // room for two packages in total
	for _, e := range []*env{a, b} {
		e.eng.defaults.Concurrency = 4
		e.eng.UseDiskBudget(shared)
	}

	var wg sync.WaitGroup
	for _, e := range []*env{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := e.mustRun(); res.Downloaded != 6 {
				t.Errorf("result = %+v", res)
			}
		}()
	}
	wg.Wait()
	if got := probe.max.Load(); got > 2 {
		t.Fatalf("%d downloads in flight across two repos; the shared budget allows 2", got)
	}
}

// Without sharing (the old behaviour) the same load overshoots: this guards
// the test above against passing for the wrong reason.
func TestSeparateBudgetsAddUp(t *testing.T) {
	probe := &concurrencyProbe{hold: 60 * time.Millisecond}
	a := newEnv(t, probe.wrap)
	b := newEnv(t, probe.wrap)
	a.publishUpstream(manyPackages(a, 6)...)
	b.publishUpstream(manyPackages(b, 6)...)
	size := len(manyPackages(a, 1)[0].Bytes)
	for _, e := range []*env{a, b} {
		e.eng.defaults.Concurrency = 4
		e.eng.UseDiskBudget(NewDiskBudget(int64(2*size + 50)))
	}
	var wg sync.WaitGroup
	for _, e := range []*env{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); e.mustRun() }()
	}
	wg.Wait()
	if got := probe.max.Load(); got < 3 {
		t.Skipf("runs did not overlap enough to show the effect (max %d)", got)
	}
}
