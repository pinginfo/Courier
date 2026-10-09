// Package rpm mirrors Fedora (RPM/dnf) repositories.
package rpm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"golang.org/x/sync/errgroup"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/pgp"
	"courier/internal/provider"
	"courier/internal/provider/hashutil"
)

const (
	maxRepomdSize = 16 << 20
	indexWorkers  = 4
)

// Provider implements provider.Provider for Fedora repositories.
type Provider struct {
	repo    config.Repo
	fp      *fetcher.Fetcher
	log     *slog.Logger
	keyring openpgp.EntityList // nil when repomd.xml.asc verification is off
}

// New builds the provider, loading gpg_keyring if one is configured.
func New(repo config.Repo, f *fetcher.Fetcher, log *slog.Logger) (*Provider, error) {
	p := &Provider{repo: repo, fp: f, log: log}
	if repo.GPGKeyring == "" {
		log.Info("gpg_keyring not set: repomd.xml signatures are not checked (Fedora does not sign its repository metadata; "+
			"integrity relies on the HTTPS upstream, the checksums in repomd.xml and primary.xml, and dnf's per-package signature check on clients)",
			"repo", repo.Name)
		return p, nil
	}
	kr, err := pgp.LoadKeyring(repo.GPGKeyring)
	if err != nil {
		return nil, err
	}
	p.keyring = kr
	return p, nil
}

// repoDir maps a (release, repo alias, arch) to the directory under the upstream
// root, following Fedora's layout.
func repoDir(release, repo, arch string) (string, error) {
	switch {
	case release == "rawhide" && repo == "releases":
		return "development/rawhide/Everything/" + arch + "/os", nil
	case release == "rawhide":
		return "", fmt.Errorf("rawhide only has the %q repo", "releases")
	case repo == "releases":
		return "releases/" + release + "/Everything/" + arch + "/os", nil
	case repo == "updates":
		return "updates/" + release + "/Everything/" + arch, nil
	case repo == "updates-testing":
		return "updates/testing/" + release + "/Everything/" + arch, nil
	default:
		return "", fmt.Errorf("unknown repo %q (want releases, updates or updates-testing)", repo)
	}
}

// Plan implements provider.Provider.
func (p *Provider) Plan(ctx context.Context) (plan *provider.Plan, err error) {
	tmp, err := os.MkdirTemp("", "courier-rpm-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	plan = &provider.Plan{Cleanup: cleanup}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	cfg := p.repo.RPM
	seen := map[string]bool{}
	for _, rel := range cfg.Releases {
		for _, name := range cfg.Repos {
			for _, arch := range cfg.Architectures {
				dir, err := repoDir(rel, name, arch)
				if err != nil {
					return nil, err
				}
				if err := p.planRepo(ctx, dir, tmp, plan, seen); err != nil {
					return nil, fmt.Errorf("%s: %w", dir, err)
				}
			}
		}
	}
	return plan, nil
}

type staged struct {
	e    repodataEntry
	file string
}

func (p *Provider) planRepo(ctx context.Context, dir, tmp string, plan *provider.Plan, seen map[string]bool) error {
	base := strings.TrimRight(p.repo.Upstream, "/") + "/" + dir

	repomdRaw, err := p.fp.Bytes(ctx, base+"/repodata/repomd.xml", maxRepomdSize)
	if err != nil {
		return fmt.Errorf("fetch repomd.xml: %w", err)
	}
	var asc []byte
	if p.keyring != nil {
		if asc, err = p.fp.Bytes(ctx, base+"/repodata/repomd.xml.asc", maxRepomdSize); err != nil {
			return fmt.Errorf("fetch repomd.xml.asc (required because gpg_keyring is set): %w", err)
		}
		if err := pgp.CheckDetached(p.keyring, repomdRaw, asc); err != nil {
			return fmt.Errorf("repomd.xml signature verification failed: %w", err)
		}
	}
	entries, err := parseRepomd(repomdRaw)
	if err != nil {
		return err
	}

	files, err := p.download(ctx, base, tmp, entries)
	if err != nil {
		return err
	}
	p.log.Info("repodata verified", "repo", p.repo.Name, "dir", dir, "files", len(files))

	var primary string
	for _, s := range files {
		if s.e.Type == "primary" {
			primary = s.file
		}
	}
	if primary == "" {
		return errors.New("repomd.xml has no primary metadata")
	}
	pkgs, err := readPrimary(primary)
	if err != nil {
		return err
	}
	for _, d := range pkgs {
		path := dir + "/" + d.Location
		if seen[path] {
			continue
		}
		seen[path] = true
		plan.Packages = append(plan.Packages, provider.Package{
			Path: path, Size: d.Size,
			Verify: func(file string) error { return hashutil.Check(file, d.ChecksumType, d.Checksum) },
		})
	}
	p.log.Info("packages loaded", "repo", p.repo.Name, "dir", dir, "packages", len(pkgs))

	// Metadata order: the checksum-named repodata files, then the signature, then
	// repomd.xml, which is what clients read first.
	for _, s := range files {
		plan.Metadata = append(plan.Metadata, provider.Metadata{Path: dir + "/" + s.e.Href, File: s.file, ContentType: "application/octet-stream"})
	}
	if asc != nil {
		plan.Metadata = append(plan.Metadata, provider.Metadata{Path: dir + "/repodata/repomd.xml.asc", Body: asc, ContentType: "application/pgp-signature"})
	}
	plan.Metadata = append(plan.Metadata, provider.Metadata{Path: dir + "/repodata/repomd.xml", Body: repomdRaw, ContentType: "application/xml"})
	return nil
}

func readPrimary(file string) ([]rpmPackage, error) {
	r, err := openDecompressed(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return parsePrimary(r)
}

// download fetches every repodata file into tmp and checks size and checksum
// against repomd.xml.
func (p *Provider) download(ctx context.Context, base, tmp string, entries []repodataEntry) ([]staged, error) {
	out := make([]staged, len(entries))
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(indexWorkers)
	for i, e := range entries {
		g.Go(func() error {
			f, err := os.CreateTemp(tmp, "repodata-*"+suffixOf(e.Href))
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			n, err := p.fp.File(gctx, base+"/"+e.Href, f)
			if err != nil {
				return fmt.Errorf("fetch %s: %w", e.Href, err)
			}
			if n != e.Size {
				return fmt.Errorf("%s: size %d does not match repomd.xml (%d)", e.Href, n, e.Size)
			}
			if err := hashutil.Check(f.Name(), e.ChecksumType, e.Checksum); err != nil {
				return fmt.Errorf("%s: %w", e.Href, err)
			}
			mu.Lock()
			out[i] = staged{e: e, file: f.Name()}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// suffixOf keeps the compression extension on the temp file name so that the
// decompressor can be chosen from it.
func suffixOf(href string) string {
	for _, ext := range []string{".xml.zst", ".xml.gz", ".xml"} {
		if strings.HasSuffix(href, ext) {
			return ext
		}
	}
	return ""
}
