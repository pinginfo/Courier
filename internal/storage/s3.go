package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"courier/internal/config"
)

// Observer receives the duration of every S3 call (op: stat|put|get|delete|ping).
type Observer func(op, status string, d time.Duration)

// S3 is a Storage backed by an S3-compatible service such as MinIO.
type S3 struct {
	client  *minio.Client
	bucket  string
	observe Observer
}

// NewS3 connects to the configured endpoint. Credentials come from the
// environment variables named in the config. observe may be nil.
func NewS3(cfg config.Storage, observe Observer) (*S3, error) {
	access, secret, err := cfg.Credentials()
	if err != nil {
		return nil, err
	}
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(access, secret, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	}
	if cfg.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	if cfg.UseSSL && cfg.CAFile != "" {
		tr, err := transportWithCA(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		opts.Transport = tr
	}
	client, err := minio.New(cfg.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	if observe == nil {
		observe = func(string, string, time.Duration) {}
	}
	return &S3{client: client, bucket: cfg.Bucket, observe: observe}, nil
}

func transportWithCA(path string) (http.RoundTripper, error) {
	pem, err := os.ReadFile(path) // #nosec G304 -- operator-provided config path
	if err != nil {
		return nil, fmt.Errorf("read ca_file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("ca_file %s contains no certificates", path)
	}
	tr, err := minio.DefaultTransport(true)
	if err != nil {
		return nil, err
	}
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return tr, nil
}

func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}

func (s *S3) done(op string, start time.Time, err error) {
	status := "ok"
	if err != nil && !errors.Is(err, ErrNotFound) {
		status = "error"
		if resp := minio.ToErrorResponse(err); resp.StatusCode != 0 {
			status = strconv.Itoa(resp.StatusCode)
		}
	}
	s.observe(op, status, time.Since(start))
}

// Stat implements Storage.
func (s *S3) Stat(ctx context.Context, key string) (info ObjectInfo, err error) {
	start := time.Now()
	defer func() { s.done("stat", start, err) }()
	st, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: st.Size}, nil
}

// Put implements Storage.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (err error) {
	start := time.Now()
	defer func() { s.done("put", start, err) }()
	_, err = s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

// Get implements Storage.
func (s *S3) Get(ctx context.Context, key string) (rc io.ReadCloser, err error) {
	start := time.Now()
	defer func() { s.done("get", start, err) }()
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// GetObject is lazy; Stat forces the request so a missing key surfaces here.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return obj, nil
}

// Delete implements Storage.
func (s *S3) Delete(ctx context.Context, key string) (err error) {
	start := time.Now()
	defer func() { s.done("delete", start, err) }()
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// Ping implements Storage.
func (s *S3) Ping(ctx context.Context) (err error) {
	start := time.Now()
	defer func() { s.done("ping", start, err) }()
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("bucket %q does not exist", s.bucket)
	}
	return nil
}
