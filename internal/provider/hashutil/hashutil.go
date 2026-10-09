// Package hashutil verifies local files against hex digests.
package hashutil

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
)

// New returns a hash for a supported algorithm name ("sha256" or "sha512").
func New(algo string) (hash.Hash, error) {
	switch algo {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("unsupported checksum type %q", algo)
	}
}

// Check hashes the file at path with algo and compares it to the hex digest want.
func Check(path, algo, want string) error {
	h, err := New(algo)
	if err != nil {
		return err
	}
	f, err := os.Open(path) // #nosec G304 G703 -- staged or temp file owned by courier
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s %s does not match expected %s", algo, got, want)
	}
	return nil
}
