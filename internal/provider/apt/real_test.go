package apt

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"courier/internal/pgp"
)

// TestRealUbuntuData checks the parsers and PGP verification against genuine,
// upstream-signed files. Opt-in: COURIER_REAL_UBUNTU must point at a directory
// with InRelease, Release, Release.gpg, ubuntu-archive-keyring.gpg and Packages.gz
// (noble main/binary-amd64).
func TestRealUbuntuData(t *testing.T) {
	dir := os.Getenv("COURIER_REAL_UBUNTU")
	if dir == "" {
		t.Skip("COURIER_REAL_UBUNTU not set")
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test input
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	kr, err := pgp.LoadKeyring(filepath.Join(dir, "ubuntu-archive-keyring.gpg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("keyring: %d entities", len(kr))

	plain, err := openInRelease(read("InRelease"), kr)
	if err != nil {
		t.Fatalf("real InRelease: %v", err)
	}
	rel, err := parseRelease(plain)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("release: suite=%s codename=%s files=%d byhash=%v", rel.Suite, rel.Codename, len(rel.Files), rel.ByHash)
	if rel.Codename != "noble" || !rel.ByHash || len(rel.Files) < 100 {
		t.Errorf("unexpected release: %+v", rel)
	}

	if err := pgp.CheckDetached(kr, read("Release"), read("Release.gpg")); err != nil {
		t.Fatalf("real Release.gpg: %v", err)
	}

	// Tampering must be caught.
	bad := read("InRelease")
	bad[len(bad)/2] ^= 0x01
	if _, err := openInRelease(bad, kr); err == nil {
		t.Error("tampered InRelease accepted")
	}
	if err := pgp.CheckDetached(kr, append(read("Release"), '\n'), read("Release.gpg")); err == nil {
		t.Error("tampered Release accepted")
	}

	// Packages.gz matches the hash in the signed Release, and parses.
	var want string
	for _, f := range rel.Files {
		if f.Path == "main/binary-amd64/Packages.gz" {
			want = f.SHA256
		}
	}
	if err := verifySHA256(want)(filepath.Join(dir, "Packages.gz")); err != nil {
		t.Fatalf("Packages.gz vs signed Release: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, "Packages.gz")) // #nosec G304 -- test input
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := parsePackages(zr)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parsed %d packages, first: %+v", len(pkgs), pkgs[0])
	if len(pkgs) != 6099 {
		t.Errorf("parsed %d packages, want 6099", len(pkgs))
	}
}
