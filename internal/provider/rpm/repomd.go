package rpm

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
)

// repodataEntry is one <data> element of repomd.xml: a metadata file that
// clients download, together with its checksum.
type repodataEntry struct {
	Type         string
	Href         string // e.g. repodata/<sum>-primary.xml.zst
	ChecksumType string
	Checksum     string
	Size         int64
}

var (
	hexRe  = regexp.MustCompile(`^[0-9a-f]{64,128}$`)
	hrefRe = regexp.MustCompile(`^repodata/[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	// Package paths: conservative characters, including those Fedora uses in
	// versions (^ ~ + %).
	pkgPathRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~%^:/-]*$`)
)

type xmlRepomd struct {
	Data []struct {
		Type     string `xml:"type,attr"`
		Checksum struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		Location struct {
			Href string `xml:"href,attr"`
			Base string `xml:"base,attr"`
		} `xml:"location"`
		Size int64 `xml:"size"`
	} `xml:"data"`
}

// parseRepomd parses repomd.xml and validates every entry that will be fetched.
func parseRepomd(raw []byte) ([]repodataEntry, error) {
	var doc xmlRepomd
	// #nosec G709 -- encoding/xml does not resolve external entities, and raw is size-limited by the fetcher.
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse repomd.xml: %w", err)
	}
	if len(doc.Data) == 0 {
		return nil, fmt.Errorf("repomd.xml lists no metadata files")
	}
	seen := map[string]bool{}
	var out []repodataEntry
	for _, d := range doc.Data {
		e := repodataEntry{
			Type: d.Type, Href: strings.TrimSpace(d.Location.Href),
			ChecksumType: d.Checksum.Type, Checksum: strings.ToLower(strings.TrimSpace(d.Checksum.Value)), Size: d.Size,
		}
		switch {
		case d.Location.Base != "":
			return nil, fmt.Errorf("repomd.xml entry %q uses xml:base, which is not supported", e.Type)
		case !hrefRe.MatchString(e.Href):
			return nil, fmt.Errorf("repomd.xml entry %q has an unsafe location %q", e.Type, e.Href)
		case e.ChecksumType != "sha256" && e.ChecksumType != "sha512":
			return nil, fmt.Errorf("repomd.xml entry %q uses unsupported checksum type %q", e.Type, e.ChecksumType)
		case !hexRe.MatchString(e.Checksum):
			return nil, fmt.Errorf("repomd.xml entry %q has a bad checksum", e.Type)
		case e.Size <= 0:
			return nil, fmt.Errorf("repomd.xml entry %q has no size", e.Type)
		case seen[e.Href]:
			return nil, fmt.Errorf("repomd.xml lists %q twice", e.Href)
		}
		seen[e.Href] = true
		out = append(out, e)
	}
	return out, nil
}
