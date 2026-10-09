package rpm

import (
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// rpmPackage is the part of a primary.xml <package> that Courier needs.
type rpmPackage struct {
	Name         string
	Location     string // relative to the repository root, e.g. Packages/a/acl-2.3.fc43.x86_64.rpm
	ChecksumType string
	Checksum     string
	Size         int64
}

type xmlPackage struct {
	Name     string `xml:"name"`
	Checksum struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"checksum"`
	Size struct {
		Package int64 `xml:"package,attr"`
	} `xml:"size"`
	Location struct {
		Href string `xml:"href,attr"`
		Base string `xml:"base,attr"`
	} `xml:"location"`
}

// openDecompressed opens a staged metadata file, undoing the compression named
// by its extension.
func openDecompressed(path string) (io.ReadCloser, error) {
	f, err := os.Open(path) // #nosec G304 G703 -- staged by this provider
	if err != nil {
		return nil, err
	}
	switch {
	case strings.HasSuffix(path, ".zst"), strings.HasSuffix(path, ".zstd"):
		dec, err := zstd.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return readCloser{Reader: dec, close: func() error { dec.Close(); return f.Close() }}, nil
	case strings.HasSuffix(path, ".gz"):
		zr, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return readCloser{Reader: zr, close: func() error { _ = zr.Close(); return f.Close() }}, nil
	case strings.HasSuffix(path, ".xml"):
		return f, nil
	default:
		_ = f.Close()
		return nil, fmt.Errorf("unsupported metadata compression for %s (supported: zstd, gzip, none)", path)
	}
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// parsePrimary streams a primary.xml, returning every rpm package with the
// fields needed to download and verify it.
func parsePrimary(r io.Reader) ([]rpmPackage, error) {
	dec := xml.NewDecoder(r)
	var out []rpmPackage
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse primary.xml: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "package" {
			continue
		}
		var p xmlPackage
		if err := dec.DecodeElement(&p, &start); err != nil {
			return nil, fmt.Errorf("parse primary.xml package: %w", err)
		}
		pkg := rpmPackage{
			Name: p.Name, Location: strings.TrimSpace(p.Location.Href),
			ChecksumType: p.Checksum.Type, Checksum: strings.ToLower(strings.TrimSpace(p.Checksum.Value)), Size: p.Size.Package,
		}
		switch {
		case p.Location.Base != "":
			return nil, fmt.Errorf("package %q uses xml:base, which is not supported", pkg.Name)
		case !pkgPathRe.MatchString(pkg.Location) || !noTraversal(pkg.Location):
			return nil, fmt.Errorf("package %q has an unsafe location %q", pkg.Name, pkg.Location)
		case pkg.ChecksumType != "sha256" && pkg.ChecksumType != "sha512":
			return nil, fmt.Errorf("package %q uses unsupported checksum type %q", pkg.Name, pkg.ChecksumType)
		case !hexRe.MatchString(pkg.Checksum):
			return nil, fmt.Errorf("package %q has a bad checksum", pkg.Name)
		case pkg.Size <= 0:
			return nil, fmt.Errorf("package %q has no size", pkg.Name)
		}
		out = append(out, pkg)
	}
}

func noTraversal(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
