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

func TestClassifyErrors(t *testing.T) {
	serve := func(code int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "x", code) }))
	}
	for code, want := range map[int]Class{404: NotFound, 410: NotFound, 403: Permanent, 400: Permanent, 503: Transient, 429: Transient} {
		srv := serve(code)
		_, err := New(Options{Retries: 1, Backoff: time.Millisecond}).Bytes(context.Background(), srv.URL, 10)
		srv.Close()
		got, ok := Classify(err)
		if !ok || got != want {
			t.Errorf("HTTP %d: class = %v (ok=%v), want %v", code, got, ok, want)
		}
		var se *StatusError
		if !errors.As(err, &se) || se.Code != code {
			t.Errorf("HTTP %d: StatusError no longer reachable through the classification", code)
		}
	}

	// A connection that cannot be made is transient once retries are used up.
	srv := serve(200)
	srv.Close()
	_, err := New(Options{Retries: 1, Backoff: time.Millisecond}).Bytes(context.Background(), srv.URL, 10)
	if got, ok := Classify(err); !ok || got != Transient {
		t.Errorf("connection refused: class = %v ok=%v, want transient", got, ok)
	}

	// A parent cancellation is not classified: callers check ctx.Err() themselves.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv2 := serve(200)
	defer srv2.Close()
	_, err = New(Options{Retries: 1, Backoff: time.Millisecond}).Bytes(ctx, srv2.URL, 10)
	if _, ok := Classify(err); ok {
		t.Errorf("a cancelled context must not be classified, got %v", err)
	}

	// An oversized body is permanent.
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(make([]byte, 100)) }))
	defer big.Close()
	_, err = New(Options{Retries: 1, Backoff: time.Millisecond}).Bytes(context.Background(), big.URL, 10)
	if got, ok := Classify(err); !ok || got != Permanent {
		t.Errorf("oversized body: class = %v ok=%v, want permanent", got, ok)
	}
}

func TestRetryDelayDoublesIsCappedAndJittered(t *testing.T) {
	base, limit := time.Second, 10*time.Second
	for n, nominal := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 5: 10 * time.Second, 9: 10 * time.Second} {
		lo, hi := time.Duration(float64(nominal)*0.75), time.Duration(float64(nominal)*1.25)
		seen := map[time.Duration]bool{}
		for i := 0; i < 50; i++ {
			d := retryDelay(base, limit, n)
			if d < lo || d > hi {
				t.Fatalf("retry %d: delay %v outside [%v, %v]", n, d, lo, hi)
			}
			seen[d] = true
		}
		if len(seen) < 5 {
			t.Errorf("retry %d: delays are not jittered (%d distinct values)", n, len(seen))
		}
	}
}

func TestNegativeRetriesMeansNone(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "x", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := New(Options{Retries: -1}).Bytes(context.Background(), srv.URL, 10); err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d, want a single attempt", err, calls.Load())
	}
}
