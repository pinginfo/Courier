package rpm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"courier/internal/provider/hashutil"
)

// TestRealFedoraData checks the parsers against genuine Fedora metadata.
// Opt-in: COURIER_REAL_FEDORA must point at a directory with repomd.xml and the
// primary.xml.zst it references (copied there as primary.xml.zst).
func TestRealFedoraData(t *testing.T) {
	dir := os.Getenv("COURIER_REAL_FEDORA")
	if dir == "" {
		t.Skip("COURIER_REAL_FEDORA not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "repomd.xml")) // #nosec G304 -- test input
	if err != nil {
		t.Fatal(err)
	}
	entries, err := parseRepomd(raw)
	if err != nil {
		t.Fatalf("real repomd.xml: %v", err)
	}
	types := []string{}
	var primary repodataEntry
	for _, e := range entries {
		types = append(types, e.Type)
		if e.Type == "primary" {
			primary = e
		}
	}
	t.Logf("repomd entries: %s", strings.Join(types, " "))

	file := filepath.Join(dir, "primary.xml.zst")
	if err := hashutil.Check(file, primary.ChecksumType, primary.Checksum); err != nil {
		t.Fatalf("primary.xml.zst vs repomd.xml: %v", err)
	}
	r, err := openDecompressed(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	pkgs, err := parsePrimary(r)
	if err != nil {
		t.Fatalf("real primary.xml: %v", err)
	}
	t.Logf("parsed %d packages; first: %+v", len(pkgs), pkgs[0])
	if len(pkgs) < 10000 {
		t.Errorf("only %d packages", len(pkgs))
	}
	special := 0
	for _, p := range pkgs {
		if strings.ContainsAny(p.Location, "^~+%") {
			special++
		}
		if !strings.HasPrefix(p.Location, "Packages/") || !strings.HasSuffix(p.Location, ".rpm") {
			t.Errorf("unexpected location %q", p.Location)
		}
	}
	t.Logf("%d packages have ^ ~ + %% in their path", special)
}
