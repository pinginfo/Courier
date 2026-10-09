package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/observability"
	"courier/internal/provider/apk"
	"courier/internal/provider/apk/apktest"
	"courier/internal/state"
	"courier/internal/storage"
)

const (
	dir      = "v3.20/main/x86_64"
	idxPath  = dir + "/APKINDEX.tar.gz"
	grace    = 72 * time.Hour
	idxKey   = "alpine/" + idxPath
	stateKey = "_courier/state/alpine.json"
)

type env struct {
	t     *testing.T
	key   apktest.Key
	up    *apktest.Repo
	store *storage.Memory
	eng   *Engine
	clock time.Time
}

func newEnv(t *testing.T, handler func(http.Handler) http.Handler) *env {
	t.Helper()
	key, keysDir := apktest.NewKey(t)
	up := apktest.NewRepo()
	var h http.Handler = up
	if handler != nil {
		h = handler(up)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	repo := config.Repo{
		Name: "alpine", Type: config.TypeAPK, Upstream: srv.URL, Prefix: "alpine", Schedule: "* * * * *",
		APK: &config.APK{Branches: []string{"v3.20"}, Repos: []string{"main"}, Architectures: []string{"x86_64"}, KeysDir: keysDir},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := fetcher.New(fetcher.Options{Retries: 1, Backoff: time.Millisecond})
	prov, err := apk.New(repo, f, log)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	defaults := config.Defaults{Concurrency: 4, Retention: config.Retention{GracePeriod: grace}}
	e := &env{t: t, key: key, up: up, store: store, clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	e.eng = NewEngine(repo, defaults, prov, f, store, observability.NewMetrics("test"), log)
	e.eng.now = func() time.Time { return e.clock }
	return e
}

// publishUpstream serves pkgs (and an index listing them) from the fake upstream.
func (e *env) publishUpstream(pkgs ...apktest.Pkg) {
	for _, p := range pkgs {
		e.up.Set(dir+"/"+p.Name+"-"+p.Version+".apk", p.Bytes)
	}
	e.up.Set(idxPath, apktest.BuildIndex(e.key, true, pkgs...))
}

func (e *env) run() (Result, error) {
	e.t.Helper()
	return e.eng.Run(context.Background())
}

func (e *env) mustRun() Result {
	e.t.Helper()
	res, err := e.run()
	if err != nil {
		e.t.Fatalf("run: %v", err)
	}
	return res
}

func (e *env) has(key string) bool { _, ok := e.store.Bytes(key); return ok }

func TestFirstSyncMirrorsEverything(t *testing.T) {
	e := newEnv(t, nil)
	a := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	b := apktest.BuildPkg(e.key, "musl", "2.0-r0", "bbb")
	e.publishUpstream(a, b)

	res := e.mustRun()
	if res.Planned != 2 || res.Downloaded != 2 || res.Skipped != 0 || res.Published != 1 {
		t.Fatalf("result = %+v", res)
	}
	got, _ := e.store.Bytes("alpine/" + dir + "/busybox-1.0-r0.apk")
	if string(got) != string(a.Bytes) {
		t.Error("stored package differs from upstream bytes")
	}
	idx, _ := e.store.Bytes(idxKey)
	if string(idx) != string(e.up.Get(idxPath)) {
		t.Error("published index is not byte-identical to upstream")
	}
	m, err := state.Load(context.Background(), e.store, "alpine")
	if err != nil || len(m.Packages) != 2 || len(m.Metadata) != 1 {
		t.Fatalf("manifest = %+v err=%v", m, err)
	}
}

func TestSecondSyncDownloadsNothing(t *testing.T) {
	e := newEnv(t, nil)
	a := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	e.publishUpstream(a)
	e.mustRun()
	before := e.up.Hit(dir + "/busybox-1.0-r0.apk")

	res := e.mustRun()
	if res.Downloaded != 0 || res.Skipped != 1 {
		t.Fatalf("result = %+v", res)
	}
	if e.up.Hit(dir+"/busybox-1.0-r0.apk") != before {
		t.Error("package was requested again")
	}
}

func TestRemovedPackageIsDeletedOnlyAfterGracePeriod(t *testing.T) {
	e := newEnv(t, nil)
	a := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	b := apktest.BuildPkg(e.key, "musl", "2.0-r0", "bbb")
	e.publishUpstream(a, b)
	e.mustRun()

	// b disappears upstream.
	e.publishUpstream(a)
	e.clock = e.clock.Add(time.Hour)
	if res := e.mustRun(); res.Deleted != 0 {
		t.Fatalf("deleted during grace: %+v", res)
	}
	bKey := "alpine/" + dir + "/musl-2.0-r0.apk"
	if !e.has(bKey) {
		t.Fatal("package removed before the grace period ended")
	}

	e.clock = e.clock.Add(grace + time.Hour)
	if res := e.mustRun(); res.Deleted != 1 {
		t.Fatalf("expected 1 deletion: %+v", res)
	}
	if e.has(bKey) {
		t.Error("orphan not deleted after grace period")
	}
	if !e.has("alpine/" + dir + "/busybox-1.0-r0.apk") {
		t.Error("live package deleted")
	}
	m, _ := state.Load(context.Background(), e.store, "alpine")
	if len(m.Orphans) != 0 {
		t.Errorf("orphans left: %+v", m.Orphans)
	}
}

func TestOrphanThatReturnsIsKept(t *testing.T) {
	e := newEnv(t, nil)
	a := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	b := apktest.BuildPkg(e.key, "musl", "2.0-r0", "bbb")
	e.publishUpstream(a, b)
	e.mustRun()
	e.publishUpstream(a)
	e.mustRun()
	e.publishUpstream(a, b) // back again
	e.mustRun()
	e.clock = e.clock.Add(10 * grace)
	if res := e.mustRun(); res.Deleted != 0 {
		t.Fatalf("revived package deleted: %+v", res)
	}
}

func TestGCNeverTouchesObjectsItDoesNotOwn(t *testing.T) {
	e := newEnv(t, nil)
	a := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	e.publishUpstream(a)
	if err := e.store.Put(context.Background(), "alpine/unrelated.txt", strings.NewReader("keep"), 4, ""); err != nil {
		t.Fatal(err)
	}
	e.mustRun()
	e.clock = e.clock.Add(10 * grace)
	e.mustRun()
	if !e.has("alpine/unrelated.txt") {
		t.Fatal("GC deleted an object that is not in the manifest")
	}
}

func TestFailedPackageBlocksMetadataAndResumeAdoptsUploads(t *testing.T) {
	e := newEnv(t, nil)
	a := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	b := apktest.BuildPkg(e.key, "musl", "2.0-r0", "bbb")
	e.publishUpstream(a, b)
	e.up.Delete(dir + "/musl-2.0-r0.apk") // listed in the index but missing

	res, err := e.run()
	if err == nil || res.Failed != 1 || res.Downloaded != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if e.has(idxKey) {
		t.Fatal("index published although a package failed")
	}
	if e.has(stateKey) {
		t.Fatal("manifest saved after a failed run")
	}

	// Upstream recovers; busybox was already uploaded and must be adopted, not re-fetched.
	e.up.Set(dir+"/musl-2.0-r0.apk", b.Bytes)
	hits := e.up.Hit(dir + "/busybox-1.0-r0.apk")
	res = e.mustRun()
	if res.Downloaded != 1 || res.Skipped != 1 || !e.has(idxKey) {
		t.Fatalf("resume result = %+v", res)
	}
	if e.up.Hit(dir+"/busybox-1.0-r0.apk") != hits {
		t.Error("already-uploaded package downloaded again")
	}
}

func TestTamperedPackageIsRejected(t *testing.T) {
	e := newEnv(t, nil)
	good := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	// The signed index lists the right size for the file served, but a checksum
	// computed from a different (genuine) control segment.
	evil := apktest.BuildPkg(e.key, "busybox", "6.6-r0", "aaa")
	evil.Name, evil.Version, evil.Checksum = good.Name, good.Version, good.Checksum
	e.publishUpstream(evil)

	_, err := e.run()
	if err == nil {
		t.Fatal("tampered package accepted")
	}
	if e.has("alpine/"+dir+"/busybox-1.0-r0.apk") || e.has(idxKey) {
		t.Fatal("tampered package or index was published")
	}
}

func TestWrongSizeIsRejected(t *testing.T) {
	e := newEnv(t, nil)
	a := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	e.publishUpstream(a)
	e.up.Set(dir+"/busybox-1.0-r0.apk", append(append([]byte{}, a.Bytes...), 0))
	if _, err := e.run(); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestBadIndexSignatureFailsBeforeAnyTransfer(t *testing.T) {
	e := newEnv(t, nil)
	other, _ := apktest.NewKey(t)
	a := apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa")
	e.publishUpstream(a)
	e.up.Set(idxPath, apktest.BuildIndex(other, true, a))
	if _, err := e.run(); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("err = %v", err)
	}
	if len(e.store.Keys()) != 0 {
		t.Fatalf("objects written despite bad signature: %v", e.store.Keys())
	}
}

func TestConcurrentRunIsBusy(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	e := newEnv(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "APKINDEX.tar.gz") {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-release
			}
			next.ServeHTTP(w, r)
		})
	})
	e.publishUpstream(apktest.BuildPkg(e.key, "busybox", "1.0-r0", "aaa"))

	done := make(chan error, 1)
	go func() { _, err := e.run(); done <- err }()
	<-entered
	if _, err := e.run(); !errors.Is(err, ErrBusy) {
		t.Fatalf("second run err = %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCancelledRunPublishesNothing(t *testing.T) {
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
		t.Fatal("expected error after cancel")
	}
	if e.has(idxKey) || e.has(stateKey) {
		t.Fatal("published after cancellation")
	}
}
