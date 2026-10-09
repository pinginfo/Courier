package syncer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/observability"
	"courier/internal/provider/apk"
	"courier/internal/provider/apt"
	"courier/internal/storage"
	"courier/internal/storage/storagetest"
)

// TestLiveAlpinePartial syncs the real Alpine "main" repo through the real S3
// client (against an in-process S3 server) for a few seconds, then cancels. It
// checks that real signed packages download, verify and upload, and that a
// cancelled run publishes nothing. Opt-in: set COURIER_LIVE_KEYS to a directory
// containing the Alpine *.rsa.pub keys. It downloads real data.
func TestLiveAlpinePartial(t *testing.T) {
	keys := os.Getenv("COURIER_LIVE_KEYS")
	if keys == "" {
		t.Skip("COURIER_LIVE_KEYS not set")
	}
	s3, err := storage.NewS3(storagetest.Start(t), nil)
	if err != nil {
		t.Fatal(err)
	}

	repo := config.Repo{
		Name: "alpine", Type: config.TypeAPK, Upstream: "https://dl-cdn.alpinelinux.org/alpine", Prefix: "alpine", Schedule: "* * * * *",
		APK: &config.APK{Branches: []string{"v3.20"}, Repos: []string{"main"}, Architectures: []string{"x86_64"}, KeysDir: keys},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := fetcher.New(fetcher.Options{UserAgent: "courier-live-test"})
	prov, err := apk.New(repo, f, log)
	if err != nil {
		t.Fatal(err)
	}
	m := observability.NewMetrics("test")
	eng := NewEngine(repo, config.Defaults{Concurrency: 8, Retention: config.Retention{GracePeriod: time.Hour}}, prov, f, s3, m, log)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := eng.Run(ctx)
	t.Logf("ran %v: planned=%d downloaded=%d skipped=%d failed=%d bytes=%d err=%v",
		time.Since(start).Round(time.Second), res.Planned, res.Downloaded, res.Skipped, res.Failed, res.Bytes, err)

	if err == nil {
		t.Fatal("expected the run to be cut short by the timeout")
	}
	if res.Planned < 1000 {
		t.Errorf("planned only %d packages", res.Planned)
	}
	if res.Downloaded < 10 {
		t.Errorf("only %d packages downloaded in 30s", res.Downloaded)
	}
	if res.Failed != 0 {
		t.Errorf("%d real packages failed verification", res.Failed)
	}
	if _, err := s3.Stat(context.Background(), "alpine/v3.20/main/x86_64/APKINDEX.tar.gz"); err == nil {
		t.Error("index published by a cancelled run")
	}
	if _, err := s3.Stat(context.Background(), "_courier/state/alpine.json"); err == nil {
		t.Error("manifest saved by a cancelled run")
	}
}

// TestLiveUbuntuPartial plans the real Ubuntu noble main/amd64 archive (real
// InRelease signature check against the archive keyring, real Packages.gz) and
// syncs packages for a few seconds before cancelling. Opt-in: set
// COURIER_LIVE_UBUNTU_KEYRING to ubuntu-archive-keyring.gpg.
func TestLiveUbuntuPartial(t *testing.T) {
	keyring := os.Getenv("COURIER_LIVE_UBUNTU_KEYRING")
	if keyring == "" {
		t.Skip("COURIER_LIVE_UBUNTU_KEYRING not set")
	}
	s3, err := storage.NewS3(storagetest.Start(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := config.Repo{
		Name: "ubuntu", Type: config.TypeAPT, Upstream: "https://archive.ubuntu.com/ubuntu", Prefix: "ubuntu", Schedule: "* * * * *",
		GPGKeyring: keyring,
		APT:        &config.APT{Suites: []string{"noble"}, Components: []string{"main"}, Architectures: []string{"amd64"}},
	}
	log := warnLogger(t)
	f := fetcher.New(fetcher.Options{UserAgent: "courier-live-test"})
	prov, err := apt.New(repo, f, log)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(repo, config.Defaults{Concurrency: 8, Retention: config.Retention{GracePeriod: time.Hour}}, prov, f, s3, observability.NewMetrics("test"), log)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	start := time.Now()
	res, err := eng.Run(ctx)
	t.Logf("ran %v: planned=%d downloaded=%d failed=%d bytes=%d err=%v",
		time.Since(start).Round(time.Second), res.Planned, res.Downloaded, res.Failed, res.Bytes, err)
	if err == nil {
		t.Fatal("expected the run to be cut short by the timeout")
	}
	if res.Planned != 6099 {
		t.Errorf("planned %d packages, want 6099", res.Planned)
	}
	if res.Downloaded < 10 || res.Failed != 0 {
		t.Errorf("downloaded=%d failed=%d", res.Downloaded, res.Failed)
	}
	if _, err := s3.Stat(context.Background(), "ubuntu/dists/noble/InRelease"); err == nil {
		t.Error("InRelease published by a cancelled run")
	}
}

// testLogger sends warnings and errors to the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func warnLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
