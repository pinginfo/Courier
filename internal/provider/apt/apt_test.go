package apt_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/provider"
	"courier/internal/provider/apt"
	"courier/internal/provider/apt/apttest"
)

func debs() []apttest.Deb {
	return []apttest.Deb{
		{Name: "hello", Version: "1.0", Arch: "amd64", Component: "main", Content: "hello-bytes"},
		{Name: "world", Version: "2.0", Arch: "amd64", Component: "main", Content: "world-bytes"},
		{Name: "extra", Version: "3.0", Arch: "amd64", Component: "universe", Content: "extra-bytes"},
		{Name: "arm", Version: "1.0", Arch: "arm64", Component: "main", Content: "arm-bytes"},
	}
}

func suite(name string) apttest.Suite {
	return apttest.Suite{Name: name, Components: []string{"main", "universe"}, Architectures: []string{"amd64", "arm64"}, Debs: debs()}
}

func newProvider(t *testing.T, repo *apttest.Repo, keyring string, mut func(*config.APT)) *apt.Provider {
	t.Helper()
	srv := httptest.NewServer(repo)
	t.Cleanup(srv.Close)
	cfg := &config.APT{Suites: []string{"noble"}, Components: []string{"main", "universe"}, Architectures: []string{"amd64"}}
	if mut != nil {
		mut(cfg)
	}
	p, err := apt.New(config.Repo{
		Name: "ubuntu", Type: config.TypeAPT, Upstream: srv.URL, Prefix: "ubuntu", GPGKeyring: keyring, APT: cfg,
	}, fetcher.New(fetcher.Options{Retries: 1, Backoff: 1}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func paths(m []provider.Metadata) []string {
	out := make([]string, len(m))
	for i, x := range m {
		out[i] = x.Path
	}
	return out
}

func has(m []provider.Metadata, p string) bool {
	for _, x := range m {
		if x.Path == p {
			return true
		}
	}
	return false
}

func TestPlanListsPackagesAndOrdersMetadata(t *testing.T) {
	key, ring := apttest.NewKey(t)
	repo := apttest.NewRepo()
	repo.Add(key.Build(suite("noble")))

	plan, err := newProvider(t, repo, ring, nil).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	// amd64 only: hello, world (main) and extra (universe); not the arm64 package.
	got := map[string]bool{}
	for _, p := range plan.Packages {
		got[p.Path] = true
		if p.Size == 0 || p.Verify == nil {
			t.Errorf("package %s lacks size/verify", p.Path)
		}
	}
	if len(got) != 3 || !got["pool/main/h/hello/hello_1.0_amd64.deb"] || !got["pool/universe/e/extra/extra_3.0_amd64.deb"] {
		t.Fatalf("packages = %v", got)
	}

	m := plan.Metadata
	for _, want := range []string{
		"dists/noble/main/binary-amd64/Packages", "dists/noble/main/binary-amd64/Packages.gz", "dists/noble/main/binary-amd64/Release",
		"dists/noble/universe/binary-amd64/Packages.gz", "dists/noble/main/i18n/Translation-en.gz",
		"dists/noble/Release", "dists/noble/Release.gpg", "dists/noble/InRelease",
	} {
		if !has(m, want) {
			t.Errorf("metadata missing %s", want)
		}
	}
	// by-hash copies exist for the index files.
	byHash := 0
	for _, x := range m {
		if strings.Contains(x.Path, "/by-hash/SHA256/") {
			byHash++
		}
	}
	if byHash == 0 {
		t.Error("no by-hash copies planned")
	}
	// Not selected: other arch, other language, dep11, sources, Contents (off by default).
	for _, not := range []string{"binary-arm64", "Translation-de", "dep11", "source/", "Contents"} {
		for _, p := range paths(m) {
			if strings.Contains(p, not) {
				t.Errorf("unexpected metadata %s", p)
			}
		}
	}
	// InRelease is last, and Release/Release.gpg come right before it.
	n := len(m)
	if m[n-1].Path != "dists/noble/InRelease" || m[n-2].Path != "dists/noble/Release" || m[n-3].Path != "dists/noble/Release.gpg" {
		t.Fatalf("tail order = %v", paths(m[n-3:]))
	}
	// Index files must be staged on disk, with the staged file present until Cleanup.
	for _, x := range m {
		if strings.HasSuffix(x.Path, "Packages.gz") {
			if x.File == "" {
				t.Fatal("Packages.gz not file-backed")
			}
			if _, err := os.Stat(x.File); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPlanCleanupRemovesStagedFiles(t *testing.T) {
	key, ring := apttest.NewKey(t)
	repo := apttest.NewRepo()
	repo.Add(key.Build(suite("noble")))
	plan, err := newProvider(t, repo, ring, nil).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var staged string
	for _, x := range plan.Metadata {
		if x.File != "" {
			staged = x.File
			break
		}
	}
	plan.Cleanup()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged file still exists: %v", err)
	}
}

func TestLanguagesAndContentsOptions(t *testing.T) {
	key, ring := apttest.NewKey(t)
	repo := apttest.NewRepo()
	repo.Add(key.Build(suite("noble")))
	plan, err := newProvider(t, repo, ring, func(c *config.APT) {
		c.Languages = []string{"de"}
		c.Contents = true
	}).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	if !has(plan.Metadata, "dists/noble/main/i18n/Translation-de.gz") || has(plan.Metadata, "dists/noble/main/i18n/Translation-en.gz") {
		t.Errorf("languages not honoured: %v", paths(plan.Metadata))
	}
	if !has(plan.Metadata, "dists/noble/main/Contents-amd64.gz") || !has(plan.Metadata, "dists/noble/Contents-amd64.gz") {
		t.Errorf("contents not mirrored: %v", paths(plan.Metadata))
	}
}

func TestNoByHashWhenUpstreamDoesNotAdvertiseIt(t *testing.T) {
	key, ring := apttest.NewKey(t)
	repo := apttest.NewRepo()
	s := suite("noble")
	s.NoByHash = true
	repo.Add(key.Build(s))
	plan, err := newProvider(t, repo, ring, nil).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	for _, p := range paths(plan.Metadata) {
		if strings.Contains(p, "by-hash") {
			t.Fatalf("by-hash planned without Acquire-By-Hash: %s", p)
		}
	}
}

func TestSharedPoolFilesAreListedOnce(t *testing.T) {
	key, ring := apttest.NewKey(t)
	repo := apttest.NewRepo()
	repo.Add(key.Build(suite("noble")))
	repo.Add(key.Build(suite("noble-updates")))
	plan, err := newProvider(t, repo, ring, func(c *config.APT) { c.Suites = []string{"noble", "noble-updates"} }).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	if len(plan.Packages) != 3 {
		t.Fatalf("packages = %d, want 3 (deduplicated)", len(plan.Packages))
	}
	if !has(plan.Metadata, "dists/noble-updates/InRelease") {
		t.Error("second suite missing")
	}
}

func TestSignatureFailures(t *testing.T) {
	key, ring := apttest.NewKey(t)
	other, _ := apttest.NewKey(t)

	t.Run("signed by another key", func(t *testing.T) {
		repo := apttest.NewRepo()
		repo.Add(other.Build(suite("noble")))
		if _, err := newProvider(t, repo, ring, nil).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("tampered InRelease", func(t *testing.T) {
		repo := apttest.NewRepo()
		files := key.Build(suite("noble"))
		files["dists/noble/InRelease"] = []byte(strings.Replace(string(files["dists/noble/InRelease"]), "Origin: Test", "Origin: Evil", 1))
		repo.Add(files)
		if _, err := newProvider(t, repo, ring, nil).Plan(context.Background()); err == nil {
			t.Fatal("tampered InRelease accepted")
		}
	})
	t.Run("Release.gpg does not match Release", func(t *testing.T) {
		repo := apttest.NewRepo()
		files := key.Build(suite("noble"))
		files["dists/noble/Release"] = append(files["dists/noble/Release"], '\n')
		repo.Add(files)
		if _, err := newProvider(t, repo, ring, nil).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "release.gpg") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no keyring accepts unknown signer", func(t *testing.T) {
		repo := apttest.NewRepo()
		repo.Add(other.Build(suite("noble")))
		plan, err := newProvider(t, repo, "", nil).Plan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		plan.Cleanup()
	})
}

func TestIndexFileMustMatchSignedHash(t *testing.T) {
	key, ring := apttest.NewKey(t)
	repo := apttest.NewRepo()
	repo.Add(key.Build(suite("noble")))
	// Upstream serves a Packages.gz that is not what the signed Release describes.
	repo.Set("dists/noble/main/binary-amd64/Packages.gz", append(repo.Get("dists/noble/main/binary-amd64/Packages.gz"), 0))
	if _, err := newProvider(t, repo, ring, nil).Plan(context.Background()); err == nil {
		t.Fatal("index with wrong hash/size accepted")
	}
}

func TestReleaseValidation(t *testing.T) {
	key, ring := apttest.NewKey(t)
	tests := []struct {
		name    string
		suite   func() apttest.Suite
		mut     func(*config.APT)
		wantErr string
	}{
		{"expired", func() apttest.Suite {
			s := suite("noble")
			s.ValidUntil = "Mon, 01 Jan 2001 00:00:00 UTC"
			return s
		}, nil, "expired"},
		{"wrong suite", func() apttest.Suite { return suite("jammy") }, nil, "not \"noble\""},
		{"unknown component", func() apttest.Suite { return suite("noble") }, func(c *config.APT) { c.Components = []string{"nonexistent"} }, "component"},
		{"unknown architecture", func() apttest.Suite { return suite("noble") }, func(c *config.APT) { c.Architectures = []string{"riscv64"} }, "architecture"},
		{"traversal in Release", func() apttest.Suite {
			s := suite("noble")
			s.Mangle = func(r string) string { return r + " " + strings.Repeat("a", 64) + " 1 ../../etc/passwd\n" }
			return s
		}, nil, "unsafe path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := apttest.NewRepo()
			s := tt.suite()
			files := key.Build(s)
			if s.Name == "jammy" { // upstream answers the noble URL with jammy's signed Release
				files["dists/noble/InRelease"] = files["dists/jammy/InRelease"]
				files["dists/noble/Release"] = files["dists/jammy/Release"]
				files["dists/noble/Release.gpg"] = files["dists/jammy/Release.gpg"]
			}
			repo.Add(files)
			_, err := newProvider(t, repo, ring, tt.mut).Plan(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestMaliciousPackagesIndexIsRejected(t *testing.T) {
	key, ring := apttest.NewKey(t)
	tests := map[string]apttest.Deb{
		"traversal":    {FilenameOverride: "pool/main/../../../etc/passwd"},
		"outside pool": {FilenameOverride: "dists/noble/InRelease"},
		"absolute":     {FilenameOverride: "/etc/passwd"},
		"no sha256":    {OmitSHA256: true},
	}
	for name, d := range tests {
		t.Run(name, func(t *testing.T) {
			d.Name, d.Version, d.Arch, d.Component, d.Content = "evil", "1", "amd64", "main", "x"
			s := suite("noble")
			s.Debs = []apttest.Deb{d}
			repo := apttest.NewRepo()
			repo.Add(key.Build(s)) // validly signed, so only the Packages content is wrong
			_, err := newProvider(t, repo, ring, nil).Plan(context.Background())
			if err == nil || (!strings.Contains(err.Error(), "unsafe Filename") && !strings.Contains(err.Error(), "lacks")) {
				t.Fatalf("err = %v, want a rejection of the package entry", err)
			}
		})
	}
}

func TestPackageVerifyChecksContent(t *testing.T) {
	key, ring := apttest.NewKey(t)
	repo := apttest.NewRepo()
	repo.Add(key.Build(suite("noble")))
	plan, err := newProvider(t, repo, ring, nil).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	var hello provider.Package
	for _, p := range plan.Packages {
		if strings.Contains(p.Path, "hello") {
			hello = p
		}
	}
	write := func(s string) string {
		f := t.TempDir() + "/x.deb"
		if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	if err := hello.Verify(write("hello-bytes")); err != nil {
		t.Errorf("genuine content rejected: %v", err)
	}
	if err := hello.Verify(write("evil-bytes!")); err == nil {
		t.Error("modified content accepted")
	}
}

func TestListedButUnservedUncompressedIndexesAreSkipped(t *testing.T) {
	key, ring := apttest.NewKey(t)
	repo := apttest.NewRepo()
	repo.Add(key.Build(suite("noble")))
	// Like Ubuntu: uncompressed variants are listed in Release but not served.
	repo.Delete("dists/noble/main/binary-amd64/Packages")
	repo.Delete("dists/noble/main/i18n/Translation-en")
	plan, err := newProvider(t, repo, ring, nil).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	if has(plan.Metadata, "dists/noble/main/binary-amd64/Packages") {
		t.Error("unserved uncompressed Packages was planned")
	}
	if !has(plan.Metadata, "dists/noble/main/binary-amd64/Packages.gz") {
		t.Error("Packages.gz missing")
	}
	if len(plan.Packages) != 3 {
		t.Errorf("packages = %d, want 3", len(plan.Packages))
	}
}

func TestMissingCompressedIndexIsAnError(t *testing.T) {
	key, ring := apttest.NewKey(t)
	repo := apttest.NewRepo()
	repo.Add(key.Build(suite("noble")))
	repo.Delete("dists/noble/main/binary-amd64/Packages.gz")
	if _, err := newProvider(t, repo, ring, nil).Plan(context.Background()); err == nil {
		t.Fatal("a missing Packages.gz must fail the plan")
	}
}
