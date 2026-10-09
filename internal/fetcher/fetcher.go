// Package fetcher downloads files from upstream mirrors with retries, a rate
// limit and stall detection.
package fetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// Defaults for the retry behaviour.
const (
	DefaultRetries    = 5
	DefaultBackoff    = 2 * time.Second
	DefaultMaxBackoff = time.Minute
)

// Class says what a caller should do about a failed download.
type Class int

const (
	// Transient errors (network trouble, timeouts, stalls, 5xx, 429, 408) may
	// succeed later; the fetcher already retried them.
	Transient Class = iota + 1
	// NotFound is a 404 or 410: the file is not on the server (any more).
	NotFound
	// Permanent errors will not go away by retrying (403, other 4xx, local errors).
	Permanent
)

func (c Class) String() string {
	switch c {
	case Transient:
		return "transient"
	case NotFound:
		return "not_found"
	case Permanent:
		return "permanent"
	default:
		return "unknown"
	}
}

type classified struct {
	class Class
	err   error
}

func (c *classified) Error() string { return c.err.Error() }
func (c *classified) Unwrap() error { return c.err }

// Classify reports how a download error should be treated. ok is false if err
// did not come from the fetcher (for example a parent context being cancelled).
func Classify(err error) (c Class, ok bool) {
	var ce *classified
	if errors.As(err, &ce) {
		return ce.class, true
	}
	return 0, false
}

// Observer receives the status ("200", "404", "error", ...) and latency of each
// upstream request attempt.
type Observer func(status string, d time.Duration)

// Options configures a Fetcher. Zero values pick sensible defaults.
type Options struct {
	Client      *http.Client
	UserAgent   string
	Retries     int           // attempts after the first; 0 = default (5), negative = none
	Backoff     time.Duration // first retry delay, doubled each time; default 2s
	MaxBackoff  time.Duration // cap for the retry delay; default 1m
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
	maxBack time.Duration
	stall   time.Duration
	limiter *rate.Limiter
	burst   int
	observe Observer
}

// New builds a Fetcher.
func New(o Options) *Fetcher {
	f := &Fetcher{
		client: o.Client, ua: o.UserAgent, retries: o.Retries,
		backoff: o.Backoff, maxBack: o.MaxBackoff, stall: o.StallTime, observe: o.Observe,
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
	switch {
	case f.retries == 0:
		f.retries = DefaultRetries
	case f.retries < 0:
		f.retries = 0
	}
	if f.backoff == 0 {
		f.backoff = DefaultBackoff
	}
	if f.maxBack == 0 {
		f.maxBack = DefaultMaxBackoff
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

// retryDelay is the wait before retry number n (1-based): base doubled each
// time, capped at limit, with +-25% jitter so that many downloads that failed
// together do not all retry together.
func retryDelay(base, limit time.Duration, n int) time.Duration {
	d := base
	for i := 1; i < n && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	jitter := 0.75 + rand.Float64()*0.5 // #nosec G404 -- spreading retries, not security
	return time.Duration(float64(d) * jitter)
}

func (f *Fetcher) do(ctx context.Context, url string, handle func(io.Reader) error) error {
	var err error
	for attempt := 0; attempt <= f.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryDelay(f.backoff, f.maxBack, attempt)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err = f.once(ctx, url, handle)
		if err == nil || ctx.Err() != nil {
			return err
		}
		var se *StatusError
		var pe *permanentError
		switch {
		case errors.As(err, &pe):
			return &classified{class: Permanent, err: err}
		case errors.As(err, &se) && se.Code == http.StatusNotFound, errors.As(err, &se) && se.Code == http.StatusGone:
			return &classified{class: NotFound, err: err}
		case errors.As(err, &se) && !se.retryable():
			return &classified{class: Permanent, err: err}
		}
	}
	return &classified{class: Transient, err: fmt.Errorf("after %d attempts: %w", f.retries+1, err)}
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
