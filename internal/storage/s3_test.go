package storage

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"courier/internal/config"
)

// newFakeS3 starts an in-process S3 server with the bucket "mirror" and returns
// a Storage connected to it through the real minio-go code path.
func newFakeS3(t *testing.T, obs Observer) *S3 {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("mirror"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	t.Setenv("COURIER_T_AK", "test")
	t.Setenv("COURIER_T_SK", "testtest")
	s, err := NewS3(config.Storage{
		Endpoint: strings.TrimPrefix(srv.URL, "http://"), Bucket: "mirror", Region: "us-east-1",
		PathStyle: true, AccessKeyEnv: "COURIER_T_AK", SecretKeyEnv: "COURIER_T_SK",
	}, obs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3RoundTrip(t *testing.T) {
	var mu sync.Mutex
	ops := map[string]int{}
	s := newFakeS3(t, func(op, status string, _ time.Duration) {
		mu.Lock()
		ops[op+":"+status]++
		mu.Unlock()
	})
	ctx := context.Background()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := s.Stat(ctx, "a/b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat missing: %v", err)
	}
	if _, err := s.Get(ctx, "a/b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	if err := s.Put(ctx, "a/b.txt", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, "a/b.txt")
	if err != nil || info.Size != 5 {
		t.Fatalf("stat = %+v, %v", info, err)
	}
	rc, err := s.Get(ctx, "a/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
	if err := s.Delete(ctx, "a/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "a/b.txt"); err != nil {
		t.Fatalf("deleting a missing object must succeed: %v", err)
	}
	if _, err := s.Stat(ctx, "a/b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after delete: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ops["put:ok"] != 1 || ops["ping:ok"] != 1 {
		t.Errorf("observer calls = %v", ops)
	}
}

func TestS3PingFailsForMissingBucket(t *testing.T) {
	s := newFakeS3(t, nil)
	s.bucket = "does-not-exist"
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("expected error for a missing bucket")
	}
}

func TestNewS3NeedsCredentials(t *testing.T) {
	_, err := NewS3(config.Storage{Endpoint: "x:9000", Bucket: "b", AccessKeyEnv: "COURIER_UNSET_A", SecretKeyEnv: "COURIER_UNSET_B"}, nil)
	if err == nil {
		t.Fatal("expected credentials error")
	}
}

func TestMemoryMatchesContract(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if _, err := m.Get(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	_ = m.Put(ctx, "x", strings.NewReader("1"), 1, "")
	if info, err := m.Stat(ctx, "x"); err != nil || info.Size != 1 {
		t.Fatal(info, err)
	}
	_ = m.Delete(ctx, "x")
	if _, err := m.Stat(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
