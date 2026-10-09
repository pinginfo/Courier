package apt

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// debPackage is one stanza of a Packages index.
type debPackage struct {
	Name     string
	Filename string // relative to the archive root, e.g. pool/main/a/apt/apt_2.7_amd64.deb
	Size     int64
	SHA256   string
}

// parsePackages reads a Packages index. Every package must carry Filename,
// Size and SHA256, otherwise it could not be verified.
func parsePackages(r io.Reader) ([]debPackage, error) {
	var out []debPackage
	var cur debPackage
	have := false
	flush := func() error {
		if !have {
			return nil
		}
		switch {
		case cur.Filename == "" || cur.Size <= 0 || cur.SHA256 == "":
			return fmt.Errorf("package %q lacks Filename, Size or SHA256", cur.Name)
		case !sha256Re.MatchString(cur.SHA256):
			return fmt.Errorf("package %q has a bad SHA256", cur.Name)
		case !strings.HasPrefix(cur.Filename, "pool/") || !safeRelPath(cur.Filename):
			return fmt.Errorf("package %q has an unsafe Filename %q", cur.Name, cur.Filename)
		}
		out = append(out, cur)
		cur, have = debPackage{}, false
		return nil
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' { // continuation (Description, ...)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Package":
			cur.Name, have = v, true
		case "Filename":
			cur.Filename, have = v, true
		case "Size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("package %q: bad Size %q", cur.Name, v)
			}
			cur.Size, have = n, true
		case "SHA256":
			cur.SHA256, have = strings.ToLower(v), true
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
