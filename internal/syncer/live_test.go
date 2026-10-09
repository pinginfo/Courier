package syncer

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/observability"
	"courier/internal/provider/apk"
	"courier/internal/storage"
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
	backend := s3mem.New()
	_ = backend.CreateBucket("mirror")
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	defer srv.Close()
	t.Setenv("COURIER_T_AK", "test")
	t.Setenv("COURIER_T_SK", "testtest")
	s3, err := storage.NewS3(config.Storage{
		Endpoint: strings.TrimPrefix(srv.URL, "http://"), Bucket: "mirror", Region: "us-east-1",
		PathStyle: true, AccessKeyEnv: "COURIER_T_AK", SecretKeyEnv: "COURIER_T_SK",
	}, nil)
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
