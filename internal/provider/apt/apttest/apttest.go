// Package apttest builds small, correctly signed APT repositories for tests.
package apttest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

// Key is a test signing key.
type Key struct{ Entity *openpgp.Entity }

// NewKey generates a key and writes its public half to a keyring file.
func NewKey(t testing.TB) (Key, string) {
	t.Helper()
	e, err := openpgp.NewEntity("Courier Test", "", "test@courier.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keyring.gpg")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return Key{Entity: e}, path
}

// Deb is a package file in the pool.
type Deb struct {
	Name, Version, Arch string
	Component           string
	Content             string
	// FilenameOverride and OmitSHA256 let tests build malicious indexes.
	FilenameOverride string
	OmitSHA256       bool
}

// Path returns the pool path of the package.
func (d Deb) Path() string {
	return fmt.Sprintf("pool/%s/%c/%s/%s_%s_%s.deb", d.Component, d.Name[0], d.Name, d.Name, d.Version, d.Arch)
}

// SHA256 returns the hex SHA-256 of the content.
func (d Deb) SHA256() string { return sum(d.Content) }

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Suite describes one suite to build.
type Suite struct {
	Name          string // Suite and Codename
	Components    []string
	Architectures []string
	Debs          []Deb
	NoByHash      bool
	ValidUntil    string // Release-format date, empty for none
	// Mangle, if set, edits the rendered Release text before signing.
	Mangle func(string) string
}

// Build renders the suite into files (path -> content) relative to the archive
// root, signed with k.
func (k Key) Build(s Suite) map[string][]byte {
	files := map[string][]byte{}
	type entry struct {
		path    string
		content []byte
	}
	var idx []entry
	root := "dists/" + s.Name + "/"
	add := func(path string, content []byte) {
		files[root+path] = content
		idx = append(idx, entry{path, content})
	}
	for _, d := range s.Debs {
		if d.FilenameOverride == "" {
			files[d.Path()] = []byte(d.Content)
		}
	}
	for _, a := range s.Architectures {
		add("Contents-"+a+".gz", gz("root contents "+a))
	}
	for _, c := range s.Components {
		for _, a := range s.Architectures {
			var pk strings.Builder
			for _, d := range s.Debs {
				if d.Component != c || (d.Arch != a && d.Arch != "all") {
					continue
				}
				fn, hash := d.Path(), "SHA256: "+d.SHA256()+"\n"
				if d.FilenameOverride != "" {
					fn = d.FilenameOverride
				}
				if d.OmitSHA256 {
					hash = ""
				}
				fmt.Fprintf(&pk, "Package: %s\nVersion: %s\nArchitecture: %s\nFilename: %s\nSize: %d\nMD5sum: x\n%sDescription: test package\n long description line\n\n",
					d.Name, d.Version, d.Arch, fn, len(d.Content), hash)
			}
			add(c+"/binary-"+a+"/Packages", []byte(pk.String()))
			add(c+"/binary-"+a+"/Packages.gz", gz(pk.String()))
			add(c+"/binary-"+a+"/Release", []byte("Archive: "+s.Name+"\nComponent: "+c+"\n"))
			add(c+"/Contents-"+a+".gz", gz("contents "+a))
		}
		add(c+"/i18n/Translation-en.gz", gz("en "+c))
		add(c+"/i18n/Translation-de.gz", gz("de "+c))
		add(c+"/dep11/Components-amd64.yml.gz", gz("dep11"))
		add(c+"/source/Sources.gz", gz("sources"))
	}
	sort.Slice(idx, func(i, j int) bool { return idx[i].path < idx[j].path })

	var rel strings.Builder
	fmt.Fprintf(&rel, "Origin: Test\nSuite: %s\nCodename: %s\nDate: Thu, 25 Apr 2024  5:10:33 UTC\n", s.Name, s.Name)
	if s.ValidUntil != "" {
		fmt.Fprintf(&rel, "Valid-Until: %s\n", s.ValidUntil)
	}
	fmt.Fprintf(&rel, "Architectures: %s\nComponents: %s\n", strings.Join(s.Architectures, " "), strings.Join(s.Components, " "))
	if !s.NoByHash {
		rel.WriteString("Acquire-By-Hash: yes\n")
	}
	rel.WriteString("MD5Sum:\n")
	for _, e := range idx {
		fmt.Fprintf(&rel, " 00000000000000000000000000000000 %d %s\n", len(e.content), e.path)
	}
	rel.WriteString("SHA256:\n")
	for _, e := range idx {
		fmt.Fprintf(&rel, " %s %d %s\n", sum(string(e.content)), len(e.content), e.path)
	}
	text := rel.String()
	if s.Mangle != nil {
		text = s.Mangle(text)
	}
	files[root+"Release"] = []byte(text)
	files[root+"InRelease"] = k.clearsign(text)
	files[root+"Release.gpg"] = k.detach(text)
	return files
}

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.Bytes()
}

func (k Key) clearsign(text string) []byte {
	var b bytes.Buffer
	w, err := clearsign.Encode(&b, k.Entity.PrivateKey, nil)
	if err != nil {
		panic(err)
	}
	_, _ = w.Write([]byte(text))
	_ = w.Close()
	return b.Bytes()
}

func (k Key) detach(text string) []byte {
	var b bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&b, k.Entity, strings.NewReader(text), nil); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// Repo is an in-memory upstream archive served over HTTP.
type Repo struct {
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
}

// NewRepo returns an empty Repo.
func NewRepo() *Repo { return &Repo{files: map[string][]byte{}, hits: map[string]int{}} }

// Add stores files (path -> content).
func (r *Repo) Add(files map[string][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, c := range files {
		r.files[p] = c
	}
}

// Set stores one file.
func (r *Repo) Set(path string, content []byte) { r.Add(map[string][]byte{path: content}) }

// Delete removes a file.
func (r *Repo) Delete(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, path)
}

// Get returns a file's content (nil if absent).
func (r *Repo) Get(path string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files[path]
}

// Hit returns how many times path was requested.
func (r *Repo) Hit(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[path]
}

// ServeHTTP implements http.Handler.
func (r *Repo) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p := strings.TrimPrefix(req.URL.Path, "/")
	r.mu.Lock()
	c, ok := r.files[p]
	r.hits[p]++
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(c)
}
