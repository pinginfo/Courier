// Package rpmtest builds small Fedora-style repositories for tests.
package rpmtest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/klauspost/compress/zstd"
)

// Rpm is a package in the repository.
type Rpm struct {
	Name, Version, Arch string
	Content             string

	LocationOverride string // for malicious-index tests
	BadChecksumType  string
}

// Location returns the path of the package relative to the repo root.
func (r Rpm) Location() string {
	if r.LocationOverride != "" {
		return r.LocationOverride
	}
	return fmt.Sprintf("Packages/%c/%s-%s.%s.rpm", r.Name[0], r.Name, r.Version, r.Arch)
}

// SHA256 returns the hex SHA-256 of the content.
func (r Rpm) SHA256() string { return Sum(r.Content) }

// Sum returns the hex SHA-256 of s.
func Sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Repo describes one repository directory.
type Repo struct {
	Dir     string // e.g. releases/43/Everything/x86_64/os
	Rpms    []Rpm
	Gzip    bool // compress with gzip instead of zstd
	Extra   map[string]string
	Mangle  func(repomd string) string
	Sign    *openpgp.Entity // if set, repomd.xml.asc is produced
	WrongSz bool            // advertise a wrong size for primary
}

// Build renders the repository into files (path relative to the archive root).
func (r Repo) Build() map[string][]byte {
	files := map[string][]byte{}
	for _, p := range r.Rpms {
		if p.LocationOverride == "" {
			files[r.Dir+"/"+p.Location()] = []byte(p.Content)
		}
	}
	var pri strings.Builder
	pri.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="` + fmt.Sprint(len(r.Rpms)) + `">` + "\n")
	for _, p := range r.Rpms {
		ct := "sha256"
		if p.BadChecksumType != "" {
			ct = p.BadChecksumType
		}
		fmt.Fprintf(&pri, `<package type="rpm"><name>%s</name><arch>%s</arch><version epoch="0" ver="1" rel="1"/>`+
			`<checksum type="%s" pkgid="YES">%s</checksum><summary>s</summary><size package="%d" installed="1" archive="1"/>`+
			`<location href="%s"/><format><rpm:license>MIT</rpm:license><rpm:provides><rpm:entry name="%s"/></rpm:provides></format></package>`+"\n",
			p.Name, p.Arch, ct, p.SHA256(), len(p.Content), p.Location(), p.Name)
	}
	pri.WriteString("</metadata>\n")

	type data struct{ typ, name, content string }
	datas := []data{
		{"primary", "primary", pri.String()},
		{"filelists", "filelists", "<filelists/>"},
		{"other", "other", "<otherdata/>"},
		{"group", "comps", "<comps/>"},
	}
	var md strings.Builder
	md.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">` + "\n  <revision>1</revision>\n")
	for _, d := range datas {
		body, ext := compress(d.content, r.Gzip, d.typ != "group")
		sum := Sum(string(body))
		name := sum + "-" + d.name + ext
		files[r.Dir+"/repodata/"+name] = body
		size := len(body)
		if d.typ == "primary" && r.WrongSz {
			size++
		}
		fmt.Fprintf(&md, "  <data type=%q>\n    <checksum type=\"sha256\">%s</checksum>\n    <open-checksum type=\"sha256\">%s</open-checksum>\n    <location href=\"repodata/%s\"/>\n    <timestamp>1</timestamp>\n    <size>%d</size>\n  </data>\n",
			d.typ, sum, Sum(d.content), name, size)
	}
	md.WriteString("</repomd>\n")
	text := md.String()
	if r.Mangle != nil {
		text = r.Mangle(text)
	}
	files[r.Dir+"/repodata/repomd.xml"] = []byte(text)
	if r.Sign != nil {
		var b bytes.Buffer
		if err := openpgp.ArmoredDetachSign(&b, r.Sign, strings.NewReader(text), nil); err != nil {
			panic(err)
		}
		files[r.Dir+"/repodata/repomd.xml.asc"] = b.Bytes()
	}
	return files
}

func compress(s string, useGzip, compressIt bool) ([]byte, string) {
	if !compressIt {
		return []byte(s), ".xml"
	}
	var b bytes.Buffer
	if useGzip {
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes(), ".xml.gz"
	}
	w, _ := zstd.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.Bytes(), ".xml.zst"
}

// NewKey returns a signing entity and the path of a keyring file holding its public key.
func NewKey(t testing.TB) (*openpgp.Entity, string) {
	t.Helper()
	e, err := openpgp.NewEntity("Courier Test", "", "test@courier.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/keyring.gpg"
	if err := writeFile(path, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	return e, path
}

// Server is an in-memory upstream served over HTTP.
type Server struct {
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
}

// NewServer returns an empty Server.
func NewServer() *Server { return &Server{files: map[string][]byte{}, hits: map[string]int{}} }

// Add stores files.
func (s *Server) Add(files map[string][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, c := range files {
		s.files[p] = c
	}
}

// Set stores one file.
func (s *Server) Set(p string, c []byte) { s.Add(map[string][]byte{p: c}) }

// Delete removes a file.
func (s *Server) Delete(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, p)
}

// Get returns a file (nil if absent).
func (s *Server) Get(p string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[p]
}

// Hit returns how often p was requested.
func (s *Server) Hit(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[p]
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	s.mu.Lock()
	c, ok := s.files[p]
	s.hits[p]++
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(c)
}
