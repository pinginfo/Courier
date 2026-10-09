package syncer

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/observability"
	"courier/internal/provider/rpm"
	"courier/internal/provider/rpm/rpmtest"
	"courier/internal/state"
	"courier/internal/storage"
)

const fedDir = "releases/43/Everything/x86_64/os"

type rpmEnv struct {
	t     *testing.T
	up    *rpmtest.Server
	store *orderStore
	eng   *Engine
	clock time.Time
}

func newRPMEnv(t *testing.T) *rpmEnv {
	t.Helper()
	up := rpmtest.NewServer()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	repo := config.Repo{
		Name: "fedora", Type: config.TypeRPM, Upstream: srv.URL, Prefix: "fedora", Schedule: "* * * * *",
		RPM: &config.RPM{Releases: []string{"43"}, Repos: []string{"releases"}, Architectures: []string{"x86_64"}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := fetcher.New(fetcher.Options{Retries: 1, Backoff: time.Millisecond})
	prov, err := rpm.New(repo, f, log)
	if err != nil {
		t.Fatal(err)
	}
	store := &orderStore{Memory: storage.NewMemory()}
	e := &rpmEnv{t: t, up: up, store: store, clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	defaults := config.Defaults{Concurrency: 4, Retention: config.Retention{GracePeriod: grace}}
	e.eng = NewEngine(repo, defaults, prov, f, store, observability.NewMetrics("test"), log)
	e.eng.now = func() time.Time { return e.clock }
	return e
}

func (e *rpmEnv) publish(rpms ...rpmtest.Rpm) {
	e.up.Add(rpmtest.Repo{Dir: fedDir, Rpms: rpms}.Build())
}

func (e *rpmEnv) run() (Result, error) { return e.eng.Run(context.Background()) }

func (e *rpmEnv) mustRun() Result {
	e.t.Helper()
	res, err := e.run()
	if err != nil {
		e.t.Fatalf("run: %v", err)
	}
	return res
}

func (e *rpmEnv) has(key string) bool { _, ok := e.store.Bytes(key); return ok }

func rpmPkg(name, ver, content string) rpmtest.Rpm {
	return rpmtest.Rpm{Name: name, Version: ver, Arch: "x86_64", Content: content}
}

func TestRPMFirstSyncPublishesRepomdLast(t *testing.T) {
	e := newRPMEnv(t)
	a, b := rpmPkg("acl", "2.3-1.fc43", "aaa"), rpmPkg("bash", "5.2-1.fc43", "bbb")
	e.publish(a, b)

	res := e.mustRun()
	if res.Planned != 2 || res.Downloaded != 2 || res.Published != 5 {
		t.Fatalf("result = %+v", res)
	}
	got, _ := e.store.Bytes("fedora/" + fedDir + "/" + a.Location())
	if string(got) != "aaa" {
		t.Error("package content differs")
	}
	repomd, _ := e.store.Bytes("fedora/" + fedDir + "/repodata/repomd.xml")
	if string(repomd) != string(e.up.Get(fedDir+"/repodata/repomd.xml")) {
		t.Error("repomd.xml is not byte-identical to upstream")
	}
	puts := e.store.puts
	last, prev := puts[len(puts)-1], puts[len(puts)-2]
	if last != "_courier/state/fedora.json" || prev != "fedora/"+fedDir+"/repodata/repomd.xml" {
		t.Fatalf("tail of writes = %v", puts[len(puts)-3:])
	}
	for i, k := range puts {
		if k == "fedora/"+fedDir+"/repodata/repomd.xml" {
			for _, after := range puts[i+1:] {
				if after != "_courier/state/fedora.json" {
					t.Fatalf("%s written after repomd.xml", after)
				}
			}
		}
	}
}

func TestRPMSecondSyncSkipsAndOldRepodataIsCollected(t *testing.T) {
	e := newRPMEnv(t)
	a := rpmPkg("acl", "2.3-1.fc43", "aaa")
	e.publish(a)
	e.mustRun()
	before := e.up.Hit(fedDir + "/" + a.Location())
	if res := e.mustRun(); res.Downloaded != 0 || res.Skipped != 1 || e.up.Hit(fedDir+"/"+a.Location()) != before {
		t.Fatalf("second run = %+v", res)
	}

	// New package -> new checksum-named primary; the old one becomes an orphan.
	e.publish(a, rpmPkg("bash", "5.2-1.fc43", "bbb"))
	e.clock = e.clock.Add(time.Hour)
	e.mustRun()
	m, _ := state.Load(context.Background(), e.store, "fedora")
	if len(m.Orphans) == 0 {
		t.Fatal("old repodata not tracked as orphans")
	}
	e.clock = e.clock.Add(grace + time.Hour)
	if res := e.mustRun(); res.Deleted == 0 {
		t.Fatalf("old repodata not collected: %+v", res)
	}
	if !e.has("fedora/" + fedDir + "/" + a.Location()) {
		t.Error("live package deleted")
	}
}

func TestRPMTamperedPackageBlocksPublish(t *testing.T) {
	e := newRPMEnv(t)
	a := rpmPkg("acl", "2.3-1.fc43", "aaa")
	e.publish(a)
	e.up.Set(fedDir+"/"+a.Location(), []byte("xxx")) // same size, different content
	if _, err := e.run(); err == nil {
		t.Fatal("tampered package accepted")
	}
	if e.has("fedora/"+fedDir+"/"+a.Location()) || e.has("fedora/"+fedDir+"/repodata/repomd.xml") {
		t.Fatal("tampered package or repomd.xml published")
	}
}

func TestRPMDroppedUploadIsDetected(t *testing.T) {
	e := newRPMEnv(t)
	e.publish(rpmPkg("acl", "2.3-1.fc43", "aaa"), rpmPkg("bash", "5.2-1.fc43", "bbb"))
	e.eng.store = lossyStore{Memory: e.store.Memory, drop: func(k string) bool { return strings.HasSuffix(k, ".rpm") }}
	if _, err := e.run(); err == nil {
		t.Fatal("dropped uploads must fail the run")
	}
	if e.has("fedora/" + fedDir + "/repodata/repomd.xml") {
		t.Fatal("repomd.xml published although packages are missing")
	}
}
