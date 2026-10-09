// Package storage abstracts the object store that holds the mirrored files.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("object not found")

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key  string
	Size int64
}

// Storage is the subset of S3 that Courier needs. Keys are slash-separated and
// never start with a slash.
type Storage interface {
	// Stat returns ErrNotFound if the object does not exist.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// Put stores r under key. size must be the exact length of r.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get returns ErrNotFound if the object does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete succeeds if the object is already gone.
	Delete(ctx context.Context, key string) error
	// Ping checks that the store is reachable and the bucket exists.
	Ping(ctx context.Context) error
}
