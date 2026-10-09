package apt

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// releaseFile is one entry of the SHA256 section of a Release file.
type releaseFile struct {
	Path   string // relative to dists/<suite>/
	Size   int64
	SHA256 string
}

// release is the parsed content of a (verified) Release / InRelease file.
type release struct {
	Suite, Codename string
	Components      []string
	Architectures   []string
	ByHash          bool
	ValidUntil      time.Time // zero if absent
	Files           []releaseFile
}

var (
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	relPathRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~%:/-]*$`)
	validUntil = []string{"Mon, 2 Jan 2006 15:04:05 MST", "Mon, 02 Jan 2006 15:04:05 MST", "Mon, _2 Jan 2006 15:04:05 MST"}
)

// noTraversal reports whether p is a relative path without empty, "." or ".."
// segments. It is applied to every Release entry, selected or not.
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

// safeRelPath is the strict check for paths that are actually mirrored: only
// conservative characters, no traversal.
func safeRelPath(p string) bool {
	return relPathRe.MatchString(p) && noTraversal(p)
}

// parseRelease parses the cleartext of a Release file. Only the SHA256 hash
// section is used; MD5 and SHA1 are ignored.
func parseRelease(plain []byte) (*release, error) {
	r := &release{}
	sc := bufio.NewScanner(bytes.NewReader(plain))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var key string
	seen := map[string]bool{}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' { // continuation line
			if key != "SHA256" {
				continue
			}
			f := strings.Fields(line)
			if len(f) != 3 {
				return nil, fmt.Errorf("bad SHA256 line %q", line)
			}
			size, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || size < 0 {
				return nil, fmt.Errorf("bad size in %q", line)
			}
			if !sha256Re.MatchString(f[0]) {
				return nil, fmt.Errorf("bad SHA256 in %q", line)
			}
			if !noTraversal(f[2]) {
				return nil, fmt.Errorf("unsafe path %q in Release", f[2])
			}
			if seen[f[2]] {
				return nil, fmt.Errorf("duplicate Release entry %q", f[2])
			}
			seen[f[2]] = true
			r.Files = append(r.Files, releaseFile{Path: f[2], Size: size, SHA256: f[0]})
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("bad Release line %q", line)
		}
		key, v = k, strings.TrimSpace(v)
		switch k {
		case "Suite":
			r.Suite = v
		case "Codename":
			r.Codename = v
		case "Components":
			r.Components = strings.Fields(v)
		case "Architectures":
			r.Architectures = strings.Fields(v)
		case "Acquire-By-Hash":
			r.ByHash = strings.EqualFold(v, "yes")
		case "Valid-Until":
			t, err := parseDate(v)
			if err != nil {
				return nil, fmt.Errorf("bad Valid-Until %q: %w", v, err)
			}
			r.ValidUntil = t
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(r.Files) == 0 {
		return nil, errors.New("release has no SHA256 section")
	}
	return r, nil
}

func parseDate(v string) (time.Time, error) {
	var err error
	for _, layout := range validUntil {
		var t time.Time
		if t, err = time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, err
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
