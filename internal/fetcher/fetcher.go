// Package fetcher downloads files from upstream mirrors with retries, a rate
// limit and stall detection.
package fetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// Observer receives the status ("200", "404", "error", ...) and latency of each
// upstream request attempt.
type Observer func(status string, d time.Duration)

// Options configures a Fetcher. Zero values pick sensible defaults.
type Options struct {
	Client      *http.Client
	UserAgent   string
	Retries     int           // attempts after the first; default 3
	Backoff     time.Duration // first retry delay, doubled each time; default 1s
	StallTime   time.Duration // abort if no bytes arrive for this long; default 60s
	BytesPerSec int64         // 0 = unlimited
	Observe     Observer
}

// Fetcher downloads files over HTTP(S).
type Fetcher struct {
	client  *http.Client
	ua      string
	retries int
	backoff time.Duration
	stall   time.Duration
	limiter *rate.Limiter
	burst   int
	observe Observer
}

// New builds a Fetcher.
func New(o Options) *Fetcher {
	f := &Fetcher{
		client: o.Client, ua: o.UserAgent, retries: o.Retries,
		backoff: o.Backoff, stall: o.StallTime, observe: o.Observe,
	}
	if f.client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 30 * time.Second
		tr.MaxIdleConnsPerHost = 16
		f.client = &http.Client{Transport: tr}
	}
	if f.ua == "" {
		f.ua = "courier"
	}
	if f.retries == 0 {
		f.retries = 3
	}
	if f.backoff == 0 {
		f.backoff = time.Second
	}
	if f.stall == 0 {
		f.stall = 60 * time.Second
	}
	if f.observe == nil {
		f.observe = func(string, time.Duration) {}
	}
	if o.BytesPerSec > 0 {
		f.burst = int(min(o.BytesPerSec, 1<<20))
		f.limiter = rate.NewLimiter(rate.Limit(o.BytesPerSec), f.burst)
	}
	return f
}

// StatusError is returned for non-200 responses.
type StatusError struct {
	URL  string
	Code int
}

func (e *StatusError) Error() string { return fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Code) }

func (e *StatusError) retryable() bool {
	return e.Code >= 500 || e.Code == http.StatusTooManyRequests || e.Code == http.StatusRequestTimeout
}

// Bytes downloads url into memory. It fails if the body exceeds max bytes.
func (f *Fetcher) Bytes(ctx context.Context, url string, limit int64) ([]byte, error) {
	var out []byte
	err := f.do(ctx, url, func(r io.Reader) error {
		b, err := io.ReadAll(io.LimitReader(r, limit+1))
		if err != nil {
			return err
		}
		if int64(len(b)) > limit {
			return &permanentError{fmt.Errorf("GET %s: body exceeds %d bytes", url, limit)}
		}
		out = b
		return nil
	})
	return out, err
}

// File downloads url into dst, truncating it first on every attempt. It returns
// the number of bytes written.
func (f *Fetcher) File(ctx context.Context, url string, dst *os.File) (int64, error) {
	var n int64
	err := f.do(ctx, url, func(r io.Reader) error {
		if err := dst.Truncate(0); err != nil {
			return &permanentError{err}
		}
		if _, err := dst.Seek(0, io.SeekStart); err != nil {
			return &permanentError{err}
		}
		var err error
		n, err = io.Copy(dst, r)
		return err
	})
	return n, err
}

type permanentError struct{ error }

func (e *permanentError) Unwrap() error { return e.error }

func (f *Fetcher) do(ctx context.Context, url string, handle func(io.Reader) error) error {
	var err error
	delay := f.backoff
	for attempt := 0; attempt <= f.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
			delay *= 2
		}
		err = f.once(ctx, url, handle)
		if err == nil || ctx.Err() != nil {
			return err
		}
		var se *StatusError
		var pe *permanentError
		if errors.As(err, &pe) || (errors.As(err, &se) && !se.retryable()) {
			return err
		}
	}
	return fmt.Errorf("after %d attempts: %w", f.retries+1, err)
}

func (f *Fetcher) once(ctx context.Context, url string, handle func(io.Reader) error) error {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(f.stall, cancel)
	defer timer.Stop()

	req, err := http.NewRequestWithContext(actx, http.MethodGet, url, nil)
	if err != nil {
		return &permanentError{err}
	}
	req.Header.Set("User-Agent", f.ua)

	start := time.Now()
	resp, err := f.client.Do(req)
	if err != nil {
		f.observe("error", time.Since(start))
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	f.observe(strconv.Itoa(resp.StatusCode), time.Since(start))
	if resp.StatusCode != http.StatusOK {
		return &StatusError{URL: url, Code: resp.StatusCode}
	}
	return handle(&bodyReader{r: resp.Body, ctx: actx, f: f, timer: timer})
}

// bodyReader applies the rate limit and resets the stall timer on every read.
type bodyReader struct {
	r     io.Reader
	ctx   context.Context
	f     *Fetcher
	timer *time.Timer
}

func (b *bodyReader) Read(p []byte) (int, error) {
	if b.f.limiter != nil && len(p) > b.f.burst {
		p = p[:b.f.burst]
	}
	n, err := b.r.Read(p)
	if n > 0 {
		if b.f.limiter != nil {
			// Waiting for the limiter must not count as a stall.
			b.timer.Stop()
			if werr := b.f.limiter.WaitN(b.ctx, n); werr != nil {
				return n, werr
			}
		}
		b.timer.Reset(b.f.stall)
	}
	return n, err
}
