package syncer

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"courier/internal/provider"
	"courier/internal/provider/apk/apktest"
)

func retryCount(e *env) float64 {
	return testutil.ToFloat64(e.eng.metrics.RetryRounds.WithLabelValues("alpine"))
}

func replanCount(e *env) float64 {
	return testutil.ToFloat64(e.eng.metrics.Replans.WithLabelValues("alpine"))
}

// failFirst answers 503 to the first n requests for package files, then works.
func failFirst(n int32, hits *atomic.Int32) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ".apk") && hits.Add(1) <= n {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func TestShortGlitchIsAbsorbedByRequestRetries(t *testing.T) {
	var hits atomic.Int32
	e := newEnv(t, failFirst(1, &hits)) // the fetcher in tests makes 2 attempts per request
	e.publishUpstream(apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa"))
	res := e.mustRun()
	if res.Downloaded != 1 || res.RetryRounds != 0 || retryCount(e) != 0 {
		t.Fatalf("a one-off 503 must be handled inside the request: %+v", res)
	}
}

func TestOutageLongerThanRequestRetriesRecoversInALaterRound(t *testing.T) {
	var hits atomic.Int32
	// 3 packages x 2 attempts = 6 failed requests: round 1 fails completely.
	e := newEnv(t, failFirst(6, &hits))
	e.publishUpstream(
		apktest.BuildPkg(e.key, "a", "1.0-r0", "aaa"),
		apktest.BuildPkg(e.key, "b", "1.0-r0", "bbb"),
		apktest.BuildPkg(e.key, "c", "1.0-r0", "ccc"),
	)
	res := e.mustRun()
	if res.Downloaded != 3 || res.RetryRounds != 1 || retryCount(e) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if res.Replans != 0 || replanCount(e) != 0 {
		t.Errorf("a plain outage must not trigger a re-plan: %+v", res)
	}
	if !e.has(idxKey) {
		t.Error("index not published after recovery")
	}
}

func TestRetryRoundsOnlyRetryWhatFailed(t *testing.T) {
	var hits atomic.Int32
	e := newEnv(t, failFirst(6, &hits))
	pkgs := []apktest.Pkg{
		apktest.BuildPkg(e.key, "a", "1.0-r0", "aaa"),
		apktest.BuildPkg(e.key, "b", "1.0-r0", "bbb"),
		apktest.BuildPkg(e.key, "c", "1.0-r0", "ccc"),
	}
	e.publishUpstream(pkgs...)
	e.mustRun()
	// Each package was requested at most: 2 failed attempts + 1 success = 3.
	for _, p := range pkgs {
		if h := e.up.Hit(dir + "/" + p.Name + "-" + p.Version + ".apk"); h > 3 {
			t.Errorf("%s requested %d times", p.Name, h)
		}
	}
}

func TestPermanentErrorFailsFastWithoutRetryRounds(t *testing.T) {
	e := newEnv(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "busybox-1.0-r0.apk") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	e.publishUpstream(apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa"))
	res, err := e.run()
	if err == nil || !strings.Contains(err.Error(), "permanently") {
		t.Fatalf("err = %v", err)
	}
	if res.RetryRounds != 0 || retryCount(e) != 0 {
		t.Errorf("a 403 must not be retried in rounds: %+v", res)
	}
	if e.has(idxKey) {
		t.Fatal("index published")
	}
}

func TestPersistentCorruptionIsRetriedThenFailsAndNeverPublishes(t *testing.T) {
	e := newEnv(t, nil)
	good := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	evil := apktest.BuildPkg(e.key, "busybox", "6.6-r0", "aaa")
	evil.Name, evil.Version, evil.Checksum = good.Name, good.Version, good.Checksum
	e.publishUpstream(evil)

	res, err := e.run()
	if err == nil || !strings.Contains(err.Error(), "still failing") {
		t.Fatalf("err = %v", err)
	}
	if res.RetryRounds != 3 {
		t.Errorf("retry rounds = %d, want 3 (the default)", res.RetryRounds)
	}
	// 1 attempt per round (verification failures are not retried inside a request) x 4 rounds.
	if h := e.up.Hit(dir + "/busybox-1.0-r0.apk"); h != 4 {
		t.Errorf("package downloaded %d times, want 4", h)
	}
	if e.has("alpine/"+dir+"/busybox-1.0-r0.apk") || e.has(idxKey) {
		t.Fatal("corrupt package or index was published")
	}
}

func TestRetryRoundsCanBeDisabled(t *testing.T) {
	e := newEnv(t, nil)
	zero := 0
	e.eng.defaults.RetryRounds = &zero
	good := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	evil := apktest.BuildPkg(e.key, "busybox", "6.6-r0", "aaa")
	evil.Name, evil.Version, evil.Checksum = good.Name, good.Version, good.Checksum
	e.publishUpstream(evil)
	res, err := e.run()
	if err == nil || res.RetryRounds != 0 || e.up.Hit(dir+"/busybox-1.0-r0.apk") != 1 {
		t.Fatalf("err=%v res=%+v hits=%d", err, res, e.up.Hit(dir+"/busybox-1.0-r0.apk"))
	}
}

func TestVanishedPackageIsReplannedAndTheNewIndexPublished(t *testing.T) {
	var e *env
	var swapped atomic.Bool
	e = newEnv(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// While b is being fetched, upstream supersedes it: new index without b, file gone.
			if strings.HasSuffix(r.URL.Path, "b-1.0-r0.apk") && swapped.CompareAndSwap(false, true) {
				a := apktest.BuildPkg(e.key, "a", "1.0-r0", "aaa")
				c := apktest.BuildPkg(e.key, "b", "2.0-r0", "bbb2")
				e.up.Delete(dir + "/b-1.0-r0.apk")
				e.up.Set(dir+"/b-2.0-r0.apk", c.Bytes)
				e.up.Set(idxPath, apktest.BuildIndex(e.key, true, a, c))
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	e.publishUpstream(
		apktest.BuildPkg(e.key, "a", "1.0-r0", "aaa"),
		apktest.BuildPkg(e.key, "b", "1.0-r0", "bbb"),
	)
	res := e.mustRun()
	if res.Replans != 1 || replanCount(e) != 1 {
		t.Fatalf("result = %+v", res)
	}
	idx, _ := e.store.Bytes(idxKey)
	if string(idx) != string(e.up.Get(idxPath)) {
		t.Error("the published index is not the fresh one")
	}
	if !e.has("alpine/"+dir+"/b-2.0-r0.apk") || !e.has("alpine/"+dir+"/a-1.0-r0.apk") {
		t.Error("packages of the fresh index are missing")
	}
	if e.has("alpine/" + dir + "/b-1.0-r0.apk") {
		t.Error("the vanished package should not be in the bucket")
	}
}

func TestPackageListedButAlwaysMissingFailsAfterReplans(t *testing.T) {
	e := newEnv(t, nil)
	good := apktest.BuildPkg(e.key, "a", "1.0-r0", "aaa")
	ghost := apktest.BuildPkg(e.key, "ghost", "1.0-r0", "boo")
	e.publishUpstream(good, ghost)
	e.up.Delete(dir + "/ghost-1.0-r0.apk") // listed in the index, never served

	res, err := e.run()
	if err == nil || !strings.Contains(err.Error(), "still failing") {
		t.Fatalf("err = %v", err)
	}
	if res.Replans != 3 || res.RetryRounds != 3 {
		t.Errorf("res = %+v, want 3 re-plans and 3 retry rounds", res)
	}
	if e.has(idxKey) || e.has(stateKey) {
		t.Fatal("published although a listed package is missing upstream")
	}
}

func TestPlanRetriesNetworkErrorsButNotVerificationFailures(t *testing.T) {
	t.Run("transient index error", func(t *testing.T) {
		var idxHits atomic.Int32
		e := newEnv(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "APKINDEX.tar.gz") && idxHits.Add(1) <= 3 {
					http.Error(w, "down", http.StatusServiceUnavailable)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
		e.publishUpstream(apktest.BuildPkg(e.key, "a", "1.0-r0", "aaa"))
		e.mustRun()
		if idxHits.Load() < 4 {
			t.Errorf("index requested %d times; the plan was not retried", idxHits.Load())
		}
	})
	t.Run("bad signature is not retried", func(t *testing.T) {
		var idxHits atomic.Int32
		e := newEnv(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "APKINDEX.tar.gz") {
					idxHits.Add(1)
				}
				next.ServeHTTP(w, r)
			})
		})
		other, _ := apktest.NewKey(t)
		a := apktest.BuildPkg(e.key, "a", "1.0-r0", "aaa")
		e.publishUpstream(a)
		e.up.Set(idxPath, apktest.BuildIndex(other, true, a))
		if _, err := e.run(); err == nil {
			t.Fatal("expected a signature error")
		}
		if idxHits.Load() != 1 {
			t.Errorf("a signature failure was retried (%d index requests)", idxHits.Load())
		}
	})
	t.Run("404 for the index is not retried", func(t *testing.T) {
		e := newEnv(t, nil)
		if _, err := e.run(); err == nil {
			t.Fatal("expected an error: upstream has no index")
		}
		if h := e.up.Hit(idxPath); h != 1 {
			t.Errorf("index requested %d times, want 1", h)
		}
	})
}

func TestCancelDuringARetryPauseReturnsPromptly(t *testing.T) {
	e := newEnv(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ".apk") {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	e.eng.retryPause = time.Hour
	e.publishUpstream(apktest.BuildPkg(e.key, "a", "1.0-r0", "aaa"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.eng.Run(ctx); done <- err }()

	deadline := time.After(10 * time.Second)
	for retryCount(e) == 0 { // the engine is now sleeping in the pause
		select {
		case <-deadline:
			cancel()
			t.Fatal("never reached a retry pause")
		case <-time.After(5 * time.Millisecond):
		}
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not stop during the pause")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("stopping took %v", time.Since(start))
	}
	if got := testutil.ToFloat64(e.eng.metrics.SyncRuns.WithLabelValues("alpine", "cancelled")); got != 1 {
		t.Errorf("cancelled runs = %v", got)
	}
}

func TestConfirmedSetReturnsOnlyMissingPackagesInOrder(t *testing.T) {
	pkgs := []provider.Package{{Path: "a"}, {Path: "b"}, {Path: "c"}, {Path: "d"}}
	c := newConfirmedSet()
	c.add("b")
	c.add("d")
	if !c.has("b") || c.has("a") {
		t.Fatal("has() is wrong")
	}
	got := c.missing(pkgs)
	if len(got) != 2 || got[0].Path != "a" || got[1].Path != "c" {
		t.Fatalf("missing = %+v, want [a c]", got)
	}
	if len(c.missing(nil)) != 0 {
		t.Fatal("missing(nil) must be empty")
	}
}

func TestRoundPauseGrowsAndIsCapped(t *testing.T) {
	base := 30 * time.Second
	for round, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 90 * time.Second, 20: 5 * time.Minute} {
		if got := roundPause(base, round); got != want {
			t.Errorf("roundPause(%d) = %v, want %v", round, got, want)
		}
	}
}
