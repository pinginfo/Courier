package syncer

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/observability"
	"courier/internal/provider/apt"
	"courier/internal/provider/apt/apttest"
	"courier/internal/state"
	"courier/internal/storage"
)

// orderStore records the order of successful Puts.
type orderStore struct {
	*storage.Memory
	mu   sync.Mutex
	puts []string
}

func (o *orderStore) Put(ctx context.Context, key string, r io.Reader, size int64, ct string) error {
	if err := o.Memory.Put(ctx, key, r, size, ct); err != nil {
		return err
	}
	o.mu.Lock()
	o.puts = append(o.puts, key)
	o.mu.Unlock()
	return nil
}

type aptEnv struct {
	t     *testing.T
	key   apttest.Key
	up    *apttest.Repo
	store *orderStore
	eng   *Engine
	clock time.Time
}

func newAptEnv(t *testing.T) *aptEnv {
	t.Helper()
	key, ring := apttest.NewKey(t)
	up := apttest.NewRepo()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	repo := config.Repo{
		Name: "ubuntu", Type: config.TypeAPT, Upstream: srv.URL, Prefix: "ubuntu", Schedule: "* * * * *", GPGKeyring: ring,
		APT: &config.APT{Suites: []string{"noble"}, Components: []string{"main"}, Architectures: []string{"amd64"}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := fetcher.New(fetcher.Options{Retries: 1, Backoff: time.Millisecond})
	prov, err := apt.New(repo, f, log)
	if err != nil {
		t.Fatal(err)
	}
	store := &orderStore{Memory: storage.NewMemory()}
	e := &aptEnv{t: t, key: key, up: up, store: store, clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	defaults := config.Defaults{Concurrency: 4, Retention: config.Retention{GracePeriod: grace}}
	e.eng = NewEngine(repo, defaults, prov, f, store, observability.NewMetrics("test"), log)
	e.eng.now = func() time.Time { return e.clock }
	return e
}

func (e *aptEnv) publish(debs ...apttest.Deb) {
	e.up.Add(e.key.Build(apttest.Suite{Name: "noble", Components: []string{"main"}, Architectures: []string{"amd64"}, Debs: debs}))
}

func (e *aptEnv) run() (Result, error) { return e.eng.Run(context.Background()) }

func (e *aptEnv) mustRun() Result {
	e.t.Helper()
	res, err := e.run()
	if err != nil {
		e.t.Fatalf("run: %v", err)
	}
	return res
}

func (e *aptEnv) has(key string) bool { _, ok := e.store.Bytes(key); return ok }

func deb(name, ver, content string) apttest.Deb {
	return apttest.Deb{Name: name, Version: ver, Arch: "amd64", Component: "main", Content: content}
}

func TestAPTFirstSyncPublishesInReleaseLast(t *testing.T) {
	e := newAptEnv(t)
	a, b := deb("hello", "1.0", "aaa"), deb("world", "2.0", "bbb")
	e.publish(a, b)

	res := e.mustRun()
	if res.Planned != 2 || res.Downloaded != 2 {
		t.Fatalf("result = %+v", res)
	}
	for _, d := range []apttest.Deb{a, b} {
		got, _ := e.store.Bytes("ubuntu/" + d.Path())
		if string(got) != d.Content {
			t.Errorf("%s content differs", d.Path())
		}
	}
	// Everything byte-identical to upstream, including signatures.
	for _, f := range []string{"InRelease", "Release", "Release.gpg", "main/binary-amd64/Packages.gz"} {
		got, _ := e.store.Bytes("ubuntu/dists/noble/" + f)
		if string(got) != string(e.up.Get("dists/noble/"+f)) {
			t.Errorf("%s is not byte-identical to upstream", f)
		}
	}
	// by-hash copy of Packages.gz exists under its SHA-256.
	hash := ""
	for _, line := range strings.Split(string(e.up.Get("dists/noble/Release")), "\n") {
		if strings.HasSuffix(line, " main/binary-amd64/Packages.gz") && len(strings.Fields(line)[0]) == 64 {
			hash = strings.Fields(line)[0]
		}
	}
	if !e.has("ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + hash) {
		t.Error("by-hash copy missing")
	}

	// Ordering: every package, then InRelease as the very last object written
	// before the manifest.
	puts := e.store.puts
	idxIn, idxPkgA, idxPkgB := -1, -1, -1
	for i, k := range puts {
		switch k {
		case "ubuntu/dists/noble/InRelease":
			idxIn = i
		case "ubuntu/" + a.Path():
			idxPkgA = i
		case "ubuntu/" + b.Path():
			idxPkgB = i
		}
	}
	if idxIn < idxPkgA || idxIn < idxPkgB {
		t.Fatalf("InRelease written before a package: %v", puts)
	}
	if puts[len(puts)-1] != "_courier/state/ubuntu.json" || puts[len(puts)-2] != "ubuntu/dists/noble/InRelease" {
		t.Fatalf("tail of writes = %v", puts[len(puts)-3:])
	}
}

func TestAPTSecondSyncSkipsAndStaleByHashIsCollected(t *testing.T) {
	e := newAptEnv(t)
	a := deb("hello", "1.0", "aaa")
	e.publish(a)
	e.mustRun()
	before := e.up.Hit(a.Path())

	if res := e.mustRun(); res.Downloaded != 0 || res.Skipped != 1 || e.up.Hit(a.Path()) != before {
		t.Fatalf("second run = %+v", res)
	}

	// A new package changes Packages(.gz); the old index's by-hash copy becomes an orphan.
	e.publish(a, deb("world", "2.0", "bbb"))
	e.clock = e.clock.Add(time.Hour)
	e.mustRun()
	m, _ := state.Load(context.Background(), e.store, "ubuntu")
	if len(m.Orphans) == 0 {
		t.Fatal("old by-hash/index files were not tracked as orphans")
	}

	e.clock = e.clock.Add(grace + time.Hour)
	res := e.mustRun()
	if res.Deleted == 0 {
		t.Fatalf("stale by-hash files not collected: %+v", res)
	}
	if !e.has("ubuntu/" + a.Path()) {
		t.Error("live package deleted")
	}
}

func TestAPTRemovedPackageSurvivesGraceThenGoes(t *testing.T) {
	e := newAptEnv(t)
	a, b := deb("hello", "1.0", "aaa"), deb("world", "2.0", "bbb")
	e.publish(a, b)
	e.mustRun()
	e.publish(a)
	e.clock = e.clock.Add(time.Hour)
	e.mustRun()
	if !e.has("ubuntu/" + b.Path()) {
		t.Fatal("removed package deleted during grace period")
	}
	e.clock = e.clock.Add(grace + time.Hour)
	e.mustRun()
	if e.has("ubuntu/" + b.Path()) {
		t.Fatal("removed package kept past grace period")
	}
}

func TestAPTTamperedPackageBlocksPublish(t *testing.T) {
	e := newAptEnv(t)
	a := deb("hello", "1.0", "aaa")
	e.publish(a)
	e.up.Set(a.Path(), []byte("xxx")) // same size, different content: SHA-256 must catch it

	if _, err := e.run(); err == nil {
		t.Fatal("tampered package accepted")
	}
	if e.has("ubuntu/"+a.Path()) || e.has("ubuntu/dists/noble/InRelease") {
		t.Fatal("tampered package or index published")
	}
}

func TestAPTBadSignatureStopsBeforeAnyWrite(t *testing.T) {
	e := newAptEnv(t)
	other, _ := apttest.NewKey(t)
	e.up.Add(other.Build(apttest.Suite{Name: "noble", Components: []string{"main"}, Architectures: []string{"amd64"}, Debs: []apttest.Deb{deb("hello", "1.0", "aaa")}}))
	if _, err := e.run(); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("err = %v", err)
	}
	if len(e.store.Keys()) != 0 {
		t.Fatalf("writes despite bad signature: %v", e.store.Keys())
	}
}
