package apk

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRealAlpineData checks the parsers against genuine, upstream-signed files.
// It is skipped unless COURIER_REAL_ALPINE points at a directory containing
// APKINDEX.tar.gz, keys/*.pub, and optionally pkg.apk with PKG_C set to its
// "C:" value from the index.
func TestRealAlpineData(t *testing.T) {
	dir := os.Getenv("COURIER_REAL_ALPINE")
	if dir == "" {
		t.Skip("COURIER_REAL_ALPINE not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "APKINDEX.tar.gz")) // #nosec G304 -- test input
	if err != nil {
		t.Fatal(err)
	}
	ia, err := parseIndexArchive(raw)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := loadKeyRing(filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ia.verify(kr); err != nil {
		t.Fatalf("real index signature: %v", err)
	}
	entries, err := parseIndex(ia.index)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("verified real index: %d packages", len(entries))
	if len(entries) < 1000 {
		t.Errorf("only %d entries", len(entries))
	}

	want := os.Getenv("PKG_C")
	if want == "" {
		return
	}
	if err := verifyPackage(want)(filepath.Join(dir, "pkg.apk")); err != nil {
		t.Fatalf("real package control checksum: %v", err)
	}
	t.Log("verified real package control checksum")

	// Tampered signature must fail.
	ia.sig[0] ^= 0xff
	if err := ia.verify(kr); err == nil {
		t.Error("tampered real signature accepted")
	}
}
