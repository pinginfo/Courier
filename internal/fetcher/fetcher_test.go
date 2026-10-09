package fetcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fast() Options { return Options{Retries: 3, Backoff: time.Millisecond} }

func TestRetriesServerErrorsThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()
	got, err := New(fast()).Bytes(context.Background(), srv.URL, 100)
	if err != nil || string(got) != "hello" || calls.Load() != 3 {
		t.Fatalf("got %q err=%v calls=%d", got, err, calls.Load())
	}
}

func TestDoesNotRetryNotFound(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := New(fast()).Bytes(context.Background(), srv.URL, 100)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestGivesUpAfterRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := New(fast()).Bytes(context.Background(), srv.URL, 100)
	if err == nil || calls.Load() != 4 {
		t.Fatalf("err=%v calls=%d (want 4 attempts)", err, calls.Load())
	}
}

func TestBytesEnforcesLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 50)))
	}))
	defer srv.Close()
	if _, err := New(fast()).Bytes(context.Background(), srv.URL, 10); err == nil {
		t.Fatal("expected size limit error")
	}
}

func TestFileRetriesAfterTruncatedBody(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("partial"))
			return // connection closes short of Content-Length
		}
		_, _ = w.Write([]byte("complete"))
	}))
	defer srv.Close()
	f, err := os.CreateTemp(t.TempDir(), "dl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	n, err := New(fast()).File(context.Background(), srv.URL, f)
	if err != nil || n != 8 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	b, _ := os.ReadFile(f.Name())
	if string(b) != "complete" {
		t.Fatalf("file = %q, want truncated-then-rewritten content", b)
	}
}

func TestStallAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("ab"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	o := fast()
	o.Retries, o.StallTime = 1, 50*time.Millisecond
	start := time.Now()
	if _, err := New(o).Bytes(context.Background(), srv.URL, 100); err == nil {
		t.Fatal("expected stall error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("stall took %v to abort", time.Since(start))
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	o := fast()
	o.Backoff = time.Hour
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := New(o).Bytes(ctx, srv.URL, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRateLimitSlowsDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 30_000)))
	}))
	defer srv.Close()
	o := fast()
	o.BytesPerSec = 20_000 // burst 20k, so ~10k more must wait ~0.5s
	start := time.Now()
	if _, err := New(o).Bytes(context.Background(), srv.URL, 1<<20); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("download took %v; rate limit not applied", d)
	}
}
