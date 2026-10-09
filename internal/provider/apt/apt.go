// Package apt mirrors Debian/Ubuntu (APT) repositories.
package apt

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"golang.org/x/sync/errgroup"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/pgp"
	"courier/internal/provider"
	"courier/internal/provider/hashutil"
)

const (
	maxReleaseSize = 64 << 20
	indexWorkers   = 4
)

// Provider implements provider.Provider for APT repositories.
type Provider struct {
	repo    config.Repo
	fp      *fetcher.Fetcher
	log     *slog.Logger
	keyring openpgp.EntityList // nil when signature verification is off
	now     func() time.Time
}

// New builds the provider, loading gpg_keyring if one is configured.
func New(repo config.Repo, f *fetcher.Fetcher, log *slog.Logger) (*Provider, error) {
	p := &Provider{repo: repo, fp: f, log: log, now: time.Now}
	if repo.GPGKeyring == "" {
		log.Warn("gpg_keyring not set: upstream Release signatures are not verified by courier (clients still verify them)", "repo", repo.Name)
		return p, nil
	}
	kr, err := pgp.LoadKeyring(repo.GPGKeyring)
	if err != nil {
		return nil, err
	}
	p.keyring = kr
	return p, nil
}

// staged is a verified index file waiting on local disk.
type staged struct {
	rf   releaseFile
	file string
}

// Plan implements provider.Provider.
func (p *Provider) Plan(ctx context.Context) (plan *provider.Plan, err error) {
	tmp, err := os.MkdirTemp("", "courier-apt-*")
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

	seen := map[string]bool{}
	for _, suite := range p.repo.APT.Suites {
		if err := p.planSuite(ctx, suite, tmp, plan, seen); err != nil {
			return nil, fmt.Errorf("suite %s: %w", suite, err)
		}
	}
	return plan, nil
}

func (p *Provider) planSuite(ctx context.Context, suite, tmp string, plan *provider.Plan, seen map[string]bool) error {
	cfg := p.repo.APT
	base := strings.TrimRight(p.repo.Upstream, "/") + "/dists/" + suite

	inRelease, err := p.fp.Bytes(ctx, base+"/InRelease", maxReleaseSize)
	if err != nil {
		return fmt.Errorf("fetch InRelease: %w", err)
	}
	plain, err := openInRelease(inRelease, p.keyring)
	if err != nil {
		return err
	}
	rel, err := parseRelease(plain)
	if err != nil {
		return fmt.Errorf("parse InRelease: %w", err)
	}
	if err := p.checkRelease(rel, suite); err != nil {
		return err
	}

	releaseRaw, err := p.fp.Bytes(ctx, base+"/Release", maxReleaseSize)
	if err != nil {
		return fmt.Errorf("fetch Release: %w", err)
	}
	releaseGPG, err := p.fp.Bytes(ctx, base+"/Release.gpg", maxReleaseSize)
	var se *fetcher.StatusError
	switch {
	case errors.As(err, &se) && se.Code == 404:
		releaseGPG = nil // optional: InRelease carries the signature
	case err != nil:
		return fmt.Errorf("fetch Release.gpg: %w", err)
	}
	if p.keyring != nil && releaseGPG != nil {
		if err := pgp.CheckDetached(p.keyring, releaseRaw, releaseGPG); err != nil {
			return fmt.Errorf("release.gpg signature verification failed: %w", err)
		}
	}

	files, err := p.selectFiles(rel)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no index files match the configured components/architectures")
	}
	stagedFiles, err := p.download(ctx, base, suite, tmp, files)
	if err != nil {
		return err
	}

	// Package lists: one Packages variant per component and architecture.
	for _, c := range cfg.Components {
		for _, a := range cfg.Architectures {
			pkgs, err := p.readPackages(stagedFiles, c, a)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", c, a, err)
			}
			for _, d := range pkgs {
				if seen[d.Filename] {
					continue
				}
				seen[d.Filename] = true
				plan.Packages = append(plan.Packages, provider.Package{
					Path: d.Filename, Size: d.Size, Verify: verifySHA256(d.SHA256),
				})
			}
			p.log.Info("packages loaded", "repo", p.repo.Name, "suite", suite, "component", c, "arch", a, "packages", len(pkgs))
		}
	}

	// Metadata, in publication order: immutable by-hash copies, the index files,
	// then Release.gpg, Release and InRelease (which clients read first) last.
	root := "dists/" + suite + "/"
	if rel.ByHash {
		for _, s := range stagedFiles {
			plan.Metadata = append(plan.Metadata, provider.Metadata{
				Path: root + path.Dir(s.rf.Path) + "/by-hash/SHA256/" + s.rf.SHA256, File: s.file, ContentType: "application/octet-stream",
			})
		}
	}
	for _, s := range stagedFiles {
		plan.Metadata = append(plan.Metadata, provider.Metadata{Path: root + s.rf.Path, File: s.file, ContentType: "application/octet-stream"})
	}
	if releaseGPG != nil {
		plan.Metadata = append(plan.Metadata, provider.Metadata{Path: root + "Release.gpg", Body: releaseGPG, ContentType: "application/pgp-signature"})
	}
	plan.Metadata = append(plan.Metadata,
		provider.Metadata{Path: root + "Release", Body: releaseRaw, ContentType: "text/plain"},
		provider.Metadata{Path: root + "InRelease", Body: inRelease, ContentType: "text/plain"},
	)
	return nil
}

// checkRelease makes sure the signed Release describes the suite we asked for
// and offers the components and architectures that are configured.
func (p *Provider) checkRelease(rel *release, suite string) error {
	if rel.Suite != suite && rel.Codename != suite {
		return fmt.Errorf("release is for suite %q / codename %q, not %q", rel.Suite, rel.Codename, suite)
	}
	if !rel.ValidUntil.IsZero() && p.now().After(rel.ValidUntil) {
		return fmt.Errorf("release expired on %s", rel.ValidUntil.Format(time.RFC1123))
	}
	for _, c := range p.repo.APT.Components {
		if !contains(rel.Components, c) {
			return fmt.Errorf("component %q is not offered by upstream (has %v)", c, rel.Components)
		}
	}
	for _, a := range p.repo.APT.Architectures {
		if !contains(rel.Architectures, a) {
			return fmt.Errorf("architecture %q is not offered by upstream (has %v)", a, rel.Architectures)
		}
	}
	return nil
}

// selectFiles keeps the Release entries for the configured components and
// architectures: binary Packages indexes (all compressions) and their Release,
// the wanted Translation files and, optionally, Contents.
func (p *Provider) selectFiles(rel *release) ([]releaseFile, error) {
	cfg := p.repo.APT
	langs := cfg.Languages
	if len(langs) == 0 {
		langs = []string{"en"}
	}
	var out []releaseFile
	for _, f := range rel.Files {
		dir, base := path.Dir(f.Path), path.Base(f.Path)
		if dir == "." {
			dir = ""
		}
		keep := false
		for _, c := range cfg.Components {
			for _, a := range cfg.Architectures {
				if dir == c+"/binary-"+a {
					keep = true
				}
				if cfg.Contents && (dir == c || dir == "") && trimCompression(base) == "Contents-"+a {
					keep = true
				}
			}
			if dir == c+"/i18n" {
				for _, l := range langs {
					if trimCompression(base) == "Translation-"+l {
						keep = true
					}
				}
			}
		}
		if keep {
			if !safeRelPath(f.Path) {
				return nil, fmt.Errorf("unsafe path %q in Release", f.Path)
			}
			out = append(out, f)
		}
	}
	return out, nil
}

func trimCompression(name string) string {
	for _, ext := range []string{".gz", ".xz", ".bz2", ".lz4", ".zst"} {
		if s, ok := strings.CutSuffix(name, ext); ok {
			return s
		}
	}
	return name
}

// download fetches the selected index files into tmp and checks size and
// SHA256 against the signed Release.
func (p *Provider) download(ctx context.Context, base, suite, tmp string, files []releaseFile) ([]staged, error) {
	out := make([]staged, len(files))
	// Release also lists uncompressed variants (Packages, Translation-en, ...) that
	// archives such as Ubuntu do not actually serve; apt only uses them to check
	// hashes. Such an entry may be missing upstream, as long as a compressed
	// sibling is listed.
	hasCompressed := map[string]bool{}
	for _, rf := range files {
		if t := trimCompression(rf.Path); t != rf.Path {
			hasCompressed[t] = true
		}
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(indexWorkers)
	for i, rf := range files {
		g.Go(func() error {
			f, err := os.CreateTemp(tmp, "idx-*")
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			n, err := p.fp.File(gctx, base+"/"+rf.Path, f)
			var se *fetcher.StatusError
			if errors.As(err, &se) && se.Code == 404 && hasCompressed[rf.Path] {
				p.log.Debug("listed but not served upstream, skipping", "repo", p.repo.Name, "suite", suite, "file", rf.Path)
				_ = os.Remove(f.Name())
				return nil
			}
			if err != nil {
				return fmt.Errorf("fetch %s: %w", rf.Path, err)
			}
			if n != rf.Size {
				return fmt.Errorf("%s: size %d does not match Release (%d)", rf.Path, n, rf.Size)
			}
			if err := verifySHA256(rf.SHA256)(f.Name()); err != nil {
				return fmt.Errorf("%s: %w", rf.Path, err)
			}
			mu.Lock()
			out[i] = staged{rf: rf, file: f.Name()}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, s := range out {
		if s.file != "" {
			kept = append(kept, s)
		}
	}
	p.log.Info("indexes verified", "repo", p.repo.Name, "suite", suite, "files", len(kept), "skipped", len(out)-len(kept))
	return kept, nil
}

// readPackages parses Packages for one component/arch, preferring gzip (stdlib)
// over the uncompressed file.
func (p *Provider) readPackages(files []staged, comp, arch string) ([]debPackage, error) {
	dir := comp + "/binary-" + arch + "/"
	var gz, plain string
	for _, s := range files {
		switch s.rf.Path {
		case dir + "Packages.gz":
			gz = s.file
		case dir + "Packages":
			plain = s.file
		}
	}
	switch {
	case gz != "":
		f, err := os.Open(gz) // #nosec G304 G703 -- staged by this provider
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return parsePackages(zr)
	case plain != "":
		f, err := os.Open(plain) // #nosec G304 G703 -- staged by this provider
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return parsePackages(f)
	default:
		return nil, errors.New("upstream offers neither Packages.gz nor Packages (xz-only repositories are not supported yet)")
	}
}

// verifySHA256 returns a check that a local file has the given SHA-256.
func verifySHA256(want string) func(string) error {
	return func(file string) error { return hashutil.Check(file, "sha256", want) }
}
