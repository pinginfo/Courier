// Package state persists what Courier has mirrored, as a manifest per repo
// stored in the bucket itself.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"courier/internal/storage"
)

const version = 1

// Orphan is a file that is no longer referenced by the upstream metadata but has
// not been deleted yet (grace period).
type Orphan struct {
	Since time.Time `json:"since"`
}

// Manifest records the objects Courier owns for one repo. Only objects listed
// here (as packages, metadata or orphans) are ever deleted.
type Manifest struct {
	Version   int               `json:"version"`
	UpdatedAt time.Time         `json:"updated_at"`
	Packages  map[string]int64  `json:"packages"`
	Metadata  []string          `json:"metadata"`
	Orphans   map[string]Orphan `json:"orphans"`
}

// New returns an empty manifest.
func New() *Manifest {
	return &Manifest{Version: version, Packages: map[string]int64{}, Orphans: map[string]Orphan{}}
}

// Key returns the bucket key of a repo's manifest.
func Key(repo string) string { return "_courier/state/" + repo + ".json" }

// Load reads the manifest for repo. A missing manifest yields an empty one.
func Load(ctx context.Context, s storage.Storage, repo string) (*Manifest, error) {
	rc, err := s.Get(ctx, Key(repo))
	if errors.Is(err, storage.ErrNotFound) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	m := New()
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Version != version {
		return nil, fmt.Errorf("manifest version %d not supported (want %d)", m.Version, version)
	}
	if m.Packages == nil {
		m.Packages = map[string]int64{}
	}
	if m.Orphans == nil {
		m.Orphans = map[string]Orphan{}
	}
	return m, nil
}

// Save writes the manifest for repo.
func Save(ctx context.Context, s storage.Storage, repo string, m *Manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.Put(ctx, Key(repo), bytesReader(data), int64(len(data)), "application/json")
}
