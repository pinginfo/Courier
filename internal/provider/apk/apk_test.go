package apk_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/provider/apk"
	"courier/internal/provider/apk/apktest"
)

func newProvider(t *testing.T, repo *apktest.Repo, keysDir string) *apk.Provider {
	t.Helper()
	srv := httptest.NewServer(repo)
	t.Cleanup(srv.Close)
	cfg := config.Repo{
		Name: "alpine", Type: config.TypeAPK, Upstream: srv.URL, Prefix: "alpine",
		APK: &config.APK{
			Branches: []string{"v3.20"}, Repos: []string{"main"}, Architectures: []string{"x86_64"},
			KeysDir: keysDir,
		},
	}
	p, err := apk.New(cfg, fetcher.New(fetcher.Options{Retries: 1, Backoff: 1}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanVerifiesAndListsPackages(t *testing.T) {
	for _, sha256Sig := range []bool{false, true} {
		key, dir := apktest.NewKey(t)
		a := apktest.BuildPkg(key, "busybox", "1.36.1-r0", "hello")
		b := apktest.BuildPkg(key, "musl", "1.2.5-r0", "world")
		repo := apktest.NewRepo()
		repo.Set("v3.20/main/x86_64/APKINDEX.tar.gz", apktest.BuildIndex(key, sha256Sig, a, b))

		plan, err := newProvider(t, repo, dir).Plan(context.Background())
		if err != nil {
			t.Fatalf("sha256=%v: %v", sha256Sig, err)
		}
		if len(plan.Packages) != 2 || plan.Packages[0].Path != "v3.20/main/x86_64/busybox-1.36.1-r0.apk" {
			t.Fatalf("packages = %+v", plan.Packages)
		}
		if plan.Packages[0].Size != int64(len(a.Bytes)) {
			t.Errorf("size = %d, want %d", plan.Packages[0].Size, len(a.Bytes))
		}
		if len(plan.Metadata) != 1 || plan.Metadata[0].Path != "v3.20/main/x86_64/APKINDEX.tar.gz" {
			t.Fatalf("metadata = %+v", plan.Metadata)
		}
	}
}

func TestPlanRejectsBadSignatureAndUnknownKey(t *testing.T) {
	key, dir := apktest.NewKey(t)
	other, _ := apktest.NewKey(t)
	pkg := apktest.BuildPkg(key, "busybox", "1.0-r0", "x")

	repo := apktest.NewRepo()
	repo.Set("v3.20/main/x86_64/APKINDEX.tar.gz", apktest.BuildIndex(other, false, pkg)) // signed by a different key, same name
	if _, err := newProvider(t, repo, dir).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("err = %v, want signature failure", err)
	}

	// Corrupt one byte of the signed part.
	good := apktest.BuildIndex(key, true, pkg)
	good[len(good)-30] ^= 0xff
	repo.Set("v3.20/main/x86_64/APKINDEX.tar.gz", good)
	if _, err := newProvider(t, repo, dir).Plan(context.Background()); err == nil {
		t.Fatal("tampered index accepted")
	}
}

func TestPlanWithoutKeysDirSkipsSignatureCheck(t *testing.T) {
	key, _ := apktest.NewKey(t)
	repo := apktest.NewRepo()
	repo.Set("v3.20/main/x86_64/APKINDEX.tar.gz", apktest.BuildIndex(key, true, apktest.BuildPkg(key, "a", "1-r0", "x")))
	if _, err := newProvider(t, repo, "").Plan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPlanRejectsUnsafePackageName(t *testing.T) {
	key, dir := apktest.NewKey(t)
	repo := apktest.NewRepo()
	repo.Set("v3.20/main/x86_64/APKINDEX.tar.gz", apktest.BuildIndex(key, true, apktest.BuildPkg(key, "../evil", "1-r0", "x")))
	if _, err := newProvider(t, repo, dir).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("err = %v, want unsafe name error", err)
	}
}

func TestPackageVerify(t *testing.T) {
	key, dir := apktest.NewKey(t)
	pkg := apktest.BuildPkg(key, "busybox", "1.0-r0", "payload")
	repo := apktest.NewRepo()
	repo.Set("v3.20/main/x86_64/APKINDEX.tar.gz", apktest.BuildIndex(key, true, pkg))
	plan, err := newProvider(t, repo, dir).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verify := plan.Packages[0].Verify

	write := func(b []byte) string {
		p := t.TempDir() + "/f.apk"
		if err := writeFile(p, b); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := verify(write(pkg.Bytes)); err != nil {
		t.Errorf("good package rejected: %v", err)
	}
	// Different control segment => checksum mismatch.
	other := apktest.BuildPkg(key, "busybox", "9.9-r0", "payload")
	if err := verify(write(other.Bytes)); err == nil {
		t.Error("package with a different control segment accepted")
	}
	if err := verify(write([]byte("not an apk"))); err == nil {
		t.Error("garbage accepted")
	}
}
