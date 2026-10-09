// Package apk mirrors Alpine Linux repositories.
package apk

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"courier/internal/config"
	"courier/internal/fetcher"
	"courier/internal/provider"
)

const maxIndexSize = 256 << 20

// safeName restricts index-supplied names so they cannot escape the repo prefix.
var safeName = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// Provider implements provider.Provider for Alpine.
type Provider struct {
	repo  config.Repo
	fetch *fetcher.Fetcher
	log   *slog.Logger
	keys  keyRing // nil when signature verification is disabled
}

// New builds the provider, loading the trusted keys if keys_dir is configured.
func New(repo config.Repo, f *fetcher.Fetcher, log *slog.Logger) (*Provider, error) {
	p := &Provider{repo: repo, fetch: f, log: log}
	if repo.APK.KeysDir == "" {
		log.Warn("apk.keys_dir not set: upstream APKINDEX signatures are not verified by courier (clients still verify them)", "repo", repo.Name)
		return p, nil
	}
	kr, err := loadKeyRing(repo.APK.KeysDir)
	if err != nil {
		return nil, err
	}
	p.keys = kr
	return p, nil
}

// Plan implements provider.Provider.
func (p *Provider) Plan(ctx context.Context) (*provider.Plan, error) {
	plan := &provider.Plan{}
	seen := map[string]bool{}
	base := strings.TrimRight(p.repo.Upstream, "/")
	for _, branch := range p.repo.APK.Branches {
		for _, repo := range p.repo.APK.Repos {
			for _, arch := range p.repo.APK.Architectures {
				dir := branch + "/" + repo + "/" + arch
				raw, err := p.fetch.Bytes(ctx, base+"/"+dir+"/APKINDEX.tar.gz", maxIndexSize)
				if err != nil {
					return nil, fmt.Errorf("%s: fetch index: %w", dir, err)
				}
				ia, err := parseIndexArchive(raw)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", dir, err)
				}
				if p.keys != nil {
					if err := ia.verify(p.keys); err != nil {
						return nil, fmt.Errorf("%s: %w", dir, err)
					}
				}
				entries, err := parseIndex(ia.index)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", dir, err)
				}
				for _, e := range entries {
					path := dir + "/" + e.name + "-" + e.version + ".apk"
					if seen[path] {
						continue
					}
					seen[path] = true
					plan.Packages = append(plan.Packages, provider.Package{
						Path: path, Size: e.size, Verify: verifyPackage(e.checksum),
					})
				}
				p.log.Info("index loaded", "repo", p.repo.Name, "dir", dir, "packages", len(entries))
				plan.Metadata = append(plan.Metadata, provider.Metadata{
					Path: dir + "/APKINDEX.tar.gz", Body: raw, ContentType: "application/gzip",
				})
			}
		}
	}
	return plan, nil
}

type indexEntry struct {
	name, version, checksum string
	size                    int64
}

// parseIndex parses the text APKINDEX: records separated by blank lines, each
// line "X:value".
func parseIndex(data []byte) ([]indexEntry, error) {
	var out []indexEntry
	var cur indexEntry
	flush := func() error {
		if cur == (indexEntry{}) {
			return nil
		}
		switch {
		case !safeName.MatchString(cur.name) || !safeName.MatchString(cur.version):
			return fmt.Errorf("unsafe package name or version %q %q", cur.name, cur.version)
		case cur.size <= 0:
			return fmt.Errorf("package %s-%s has no size", cur.name, cur.version)
		case !strings.HasPrefix(cur.checksum, "Q1"):
			return fmt.Errorf("package %s-%s has unsupported checksum %q", cur.name, cur.version, cur.checksum)
		}
		out = append(out, cur)
		cur = indexEntry{}
		return nil
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if len(line) < 2 || line[1] != ':' {
			continue
		}
		val := line[2:]
		switch line[0] {
		case 'P':
			cur.name = val
		case 'V':
			cur.version = val
		case 'C':
			cur.checksum = val
		case 'S':
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad size %q: %w", val, err)
			}
			cur.size = n
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}
