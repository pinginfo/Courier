package rpm_test

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
	"courier/internal/provider/rpm"
	"courier/internal/provider/rpm/rpmtest"
)

const relDir = "releases/43/Everything/x86_64/os"

func rpms() []rpmtest.Rpm {
	return []rpmtest.Rpm{
		{Name: "acl", Version: "2.3-1.fc43", Arch: "x86_64", Content: "acl-bytes"},
		{Name: "bash", Version: "5.2-1.fc43", Arch: "x86_64", Content: "bash-bytes"},
		{Name: "snap", Version: "1.0^20240101git1234-1.fc43", Arch: "noarch", Content: "snap-bytes"},
	}
}

func newProvider(t *testing.T, srv *rpmtest.Server, keyring string, mut func(*config.RPM)) *rpm.Provider {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	cfg := &config.RPM{Releases: []string{"43"}, Repos: []string{"releases"}, Architectures: []string{"x86_64"}}
	if mut != nil {
		mut(cfg)
	}
	p, err := rpm.New(config.Repo{Name: "fedora", Type: config.TypeRPM, Upstream: ts.URL, Prefix: "fedora", GPGKeyring: keyring, RPM: cfg},
		fetcher.New(fetcher.Options{Retries: 1, Backoff: 1}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func has(m []provider.Metadata, p string) bool {
	for _, x := range m {
		if x.Path == p {
			return true
		}
	}
	return false
}

func TestPlanZstdAndGzip(t *testing.T) {
	for _, useGzip := range []bool{false, true} {
		srv := rpmtest.NewServer()
		srv.Add(rpmtest.Repo{Dir: relDir, Rpms: rpms(), Gzip: useGzip}.Build())
		plan, err := newProvider(t, srv, "", nil).Plan(context.Background())
		if err != nil {
			t.Fatalf("gzip=%v: %v", useGzip, err)
		}
		if len(plan.Packages) != 3 {
			t.Fatalf("gzip=%v: packages = %d", useGzip, len(plan.Packages))
		}
		var paths []string
		for _, p := range plan.Packages {
			paths = append(paths, p.Path)
			if p.Size == 0 || p.Verify == nil {
				t.Errorf("%s lacks size/verify", p.Path)
			}
		}
		if !strings.Contains(strings.Join(paths, " "), relDir+"/Packages/s/snap-1.0^20240101git1234-1.fc43.noarch.rpm") {
			t.Errorf("paths = %v", paths)
		}
		m := plan.Metadata
		if m[len(m)-1].Path != relDir+"/repodata/repomd.xml" {
			t.Errorf("repomd.xml must be published last, tail = %s", m[len(m)-1].Path)
		}
		if len(m) != 5 { // 4 repodata files + repomd.xml
			t.Errorf("metadata = %d, want 5", len(m))
		}
		for _, x := range m[:len(m)-1] {
			if x.File == "" {
				t.Errorf("%s not file-backed", x.Path)
			}
		}
		plan.Cleanup()
	}
}

func TestCleanupRemovesStagedFiles(t *testing.T) {
	srv := rpmtest.NewServer()
	srv.Add(rpmtest.Repo{Dir: relDir, Rpms: rpms()}.Build())
	plan, err := newProvider(t, srv, "", nil).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staged := plan.Metadata[0].File
	plan.Cleanup()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged file remains: %v", err)
	}
}

func TestRepoLayouts(t *testing.T) {
	srv := rpmtest.NewServer()
	for _, dir := range []string{
		"releases/43/Everything/x86_64/os", "updates/43/Everything/x86_64",
		"updates/testing/43/Everything/x86_64", "development/rawhide/Everything/x86_64/os",
	} {
		srv.Add(rpmtest.Repo{Dir: dir, Rpms: rpms()[:1]}.Build())
	}
	plan, err := newProvider(t, srv, "", func(c *config.RPM) {
		c.Repos = []string{"releases", "updates", "updates-testing"}
	}).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	for _, dir := range []string{"releases/43/Everything/x86_64/os", "updates/43/Everything/x86_64", "updates/testing/43/Everything/x86_64"} {
		if !has(plan.Metadata, dir+"/repodata/repomd.xml") {
			t.Errorf("missing %s", dir)
		}
	}
	raw, err := newProvider(t, srv, "", func(c *config.RPM) { c.Releases = []string{"rawhide"} }).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Cleanup()
	if !has(raw.Metadata, "development/rawhide/Everything/x86_64/os/repodata/repomd.xml") {
		t.Error("rawhide layout wrong")
	}
}

func TestSignatureWhenKeyringConfigured(t *testing.T) {
	key, ring := rpmtest.NewKey(t)
	other, _ := rpmtest.NewKey(t)

	t.Run("valid", func(t *testing.T) {
		srv := rpmtest.NewServer()
		srv.Add(rpmtest.Repo{Dir: relDir, Rpms: rpms(), Sign: key}.Build())
		plan, err := newProvider(t, srv, ring, nil).Plan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Cleanup()
		m := plan.Metadata
		if m[len(m)-2].Path != relDir+"/repodata/repomd.xml.asc" || m[len(m)-1].Path != relDir+"/repodata/repomd.xml" {
			t.Errorf("tail = %s, %s", m[len(m)-2].Path, m[len(m)-1].Path)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		srv := rpmtest.NewServer()
		srv.Add(rpmtest.Repo{Dir: relDir, Rpms: rpms(), Sign: other}.Build())
		if _, err := newProvider(t, srv, ring, nil).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("signature missing", func(t *testing.T) {
		srv := rpmtest.NewServer()
		srv.Add(rpmtest.Repo{Dir: relDir, Rpms: rpms()}.Build())
		if _, err := newProvider(t, srv, ring, nil).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "repomd.xml.asc") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("tampered repomd", func(t *testing.T) {
		srv := rpmtest.NewServer()
		files := rpmtest.Repo{Dir: relDir, Rpms: rpms(), Sign: key}.Build()
		files[relDir+"/repodata/repomd.xml"] = append(files[relDir+"/repodata/repomd.xml"], '\n')
		srv.Add(files)
		if _, err := newProvider(t, srv, ring, nil).Plan(context.Background()); err == nil {
			t.Fatal("tampered repomd.xml accepted")
		}
	})
}

func TestRepodataMustMatchRepomd(t *testing.T) {
	t.Run("corrupted file", func(t *testing.T) {
		srv := rpmtest.NewServer()
		files := rpmtest.Repo{Dir: relDir, Rpms: rpms()}.Build()
		for p, c := range files {
			if strings.HasSuffix(p, "filelists.xml.zst") {
				c[len(c)/2] ^= 0xff
			}
		}
		srv.Add(files)
		if _, err := newProvider(t, srv, "", nil).Plan(context.Background()); err == nil {
			t.Fatal("corrupted repodata accepted")
		}
	})
	t.Run("wrong size", func(t *testing.T) {
		srv := rpmtest.NewServer()
		srv.Add(rpmtest.Repo{Dir: relDir, Rpms: rpms(), WrongSz: true}.Build())
		if _, err := newProvider(t, srv, "", nil).Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "size") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMaliciousMetadataIsRejected(t *testing.T) {
	bad := map[string]func() rpmtest.Repo{
		"traversal in package": func() rpmtest.Repo {
			r := rpms()[:1]
			r[0].LocationOverride = "Packages/../../../etc/passwd"
			return rpmtest.Repo{Dir: relDir, Rpms: r}
		},
		"absolute package path": func() rpmtest.Repo {
			r := rpms()[:1]
			r[0].LocationOverride = "/etc/passwd"
			return rpmtest.Repo{Dir: relDir, Rpms: r}
		},
		"weak checksum": func() rpmtest.Repo {
			r := rpms()[:1]
			r[0].BadChecksumType = "md5"
			return rpmtest.Repo{Dir: relDir, Rpms: r}
		},
		"repodata path traversal": func() rpmtest.Repo {
			return rpmtest.Repo{Dir: relDir, Rpms: rpms(), Mangle: func(s string) string {
				return strings.Replace(s, `href="repodata/`, `href="repodata/../../`, 1)
			}}
		},
	}
	for name, mk := range bad {
		t.Run(name, func(t *testing.T) {
			srv := rpmtest.NewServer()
			srv.Add(mk().Build())
			if _, err := newProvider(t, srv, "", nil).Plan(context.Background()); err == nil {
				t.Fatal("malicious metadata accepted")
			}
		})
	}
}

func TestPackageVerify(t *testing.T) {
	srv := rpmtest.NewServer()
	srv.Add(rpmtest.Repo{Dir: relDir, Rpms: rpms()}.Build())
	plan, err := newProvider(t, srv, "", nil).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	var acl provider.Package
	for _, p := range plan.Packages {
		if strings.Contains(p.Path, "/acl-") {
			acl = p
		}
	}
	write := func(s string) string {
		f := t.TempDir() + "/x.rpm"
		if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	if err := acl.Verify(write("acl-bytes")); err != nil {
		t.Errorf("genuine content rejected: %v", err)
	}
	if err := acl.Verify(write("acl-evil!")); err == nil {
		t.Error("modified content accepted")
	}
}

func TestMissingPrimaryAndDuplicates(t *testing.T) {
	srv := rpmtest.NewServer()
	files := rpmtest.Repo{Dir: relDir, Rpms: rpms()}.Build()
	srv.Add(files)
	if _, err := newProvider(t, srv, "", func(c *config.RPM) { c.Releases = []string{"99"} }).Plan(context.Background()); err == nil {
		t.Fatal("a release that does not exist upstream must fail")
	}
	// Same package in two releases' dirs is two distinct paths; within one repo a package is listed once.
	srv2 := rpmtest.NewServer()
	dup := append(rpms(), rpms()[0])
	srv2.Add(rpmtest.Repo{Dir: relDir, Rpms: dup}.Build())
	plan, err := newProvider(t, srv2, "", nil).Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()
	if len(plan.Packages) != 3 {
		t.Fatalf("packages = %d, want 3 after de-duplication", len(plan.Packages))
	}
}
