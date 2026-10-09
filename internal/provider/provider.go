// Package provider defines what a repository-format plugin (APT, RPM, APK, ...)
// must supply to the sync engine.
package provider

import "context"

// Provider understands one repository format. It does no bulk transfer itself:
// it fetches and verifies the (small) metadata, then tells the engine which
// package files must exist.
type Provider interface {
	// Plan fetches and verifies upstream metadata, applies the repo's filters,
	// and returns everything that should be mirrored.
	Plan(ctx context.Context) (*Plan, error)
}

// Plan is the desired state of one repository.
type Plan struct {
	// Cleanup, if set, is called by the engine when the run ends (success or
	// failure) to remove temporary files referenced by Metadata.File.
	Cleanup func()

	// Packages are immutable payload files. Paths are relative to the upstream
	// root and to the repo's bucket prefix.
	Packages []Package
	// Metadata files are published last, in order, so that a client never sees
	// an index that references a package that is not there yet.
	Metadata []Metadata
}

// Package is one payload file to mirror.
type Package struct {
	Path string
	// Size is the expected size in bytes; 0 means unknown.
	Size int64
	// Verify, if set, checks the downloaded file at localPath. A non-nil error
	// rejects the file (it is not uploaded).
	Verify func(localPath string) error
}

// Metadata is an index file whose verified content is published as-is. The
// content is either Body (small files) or the local file at File (large files,
// which the provider has already verified).
type Metadata struct {
	Path        string
	Body        []byte
	File        string
	ContentType string
}
