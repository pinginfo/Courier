// Package apktest builds small, correctly signed Alpine repositories for tests.
package apktest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- Alpine checksums and legacy signatures are SHA-1.
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// KeyName is the file name the test key is stored under.
const KeyName = "test@courier.rsa.pub"

// Key is a test signing key.
type Key struct{ Priv *rsa.PrivateKey }

// NewKey generates a key and writes its public half into a fresh keys directory.
func NewKey(t testing.TB) (Key, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, KeyName), pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return Key{Priv: priv}, dir
}

func gzipMember(tarBytes []byte) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	_, _ = gz.Write(tarBytes)
	_ = gz.Close()
	return b.Bytes()
}

// tarOf builds a tar stream; with end=false the end-of-archive marker is
// omitted, as in real Alpine signature members.
func tarOf(end bool, files ...[2]string) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range files {
		_ = tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1]))})
		_, _ = tw.Write([]byte(f[1]))
	}
	if end {
		_ = tw.Close()
	} else {
		_ = tw.Flush()
	}
	return b.Bytes()
}

func (k Key) sign(data []byte, sha256Sig bool) (name string, sig []byte) {
	if sha256Sig {
		sum := sha256.Sum256(data)
		sig, _ = rsa.SignPKCS1v15(rand.Reader, k.Priv, crypto.SHA256, sum[:])
		return ".SIGN.RSA256." + KeyName, sig
	}
	sum := sha1.Sum(data) // #nosec G401 -- SHA-1 is mandated by the Alpine format
	sig, _ = rsa.SignPKCS1v15(rand.Reader, k.Priv, crypto.SHA1, sum[:])
	return ".SIGN.RSA." + KeyName, sig
}

// Pkg is a built .apk with its index checksum.
type Pkg struct {
	Name, Version string
	Bytes         []byte
	Checksum      string // value of the index "C:" field
}

// BuildPkg builds a signed .apk: [signature][control][data].
func BuildPkg(k Key, name, version string, payload string) Pkg {
	control := gzipMember(tarOf(false, [2]string{".PKGINFO", "pkgname = " + name + "\npkgver = " + version + "\n"}))
	sigName, sig := k.sign(control, false)
	sigMember := gzipMember(tarOf(false, [2]string{sigName, string(sig)}))
	data := gzipMember(tarOf(true, [2]string{"usr/share/" + name, payload}))
	sum := sha1.Sum(control) // #nosec G401 -- SHA-1 is mandated by the Alpine format
	all := bytes.Join([][]byte{sigMember, control, data}, nil)
	return Pkg{Name: name, Version: version, Bytes: all, Checksum: "Q1" + base64.StdEncoding.EncodeToString(sum[:])}
}

// BuildIndex builds a signed APKINDEX.tar.gz for pkgs.
func BuildIndex(k Key, sha256Sig bool, pkgs ...Pkg) []byte {
	var idx strings.Builder
	for _, p := range pkgs {
		fmt.Fprintf(&idx, "C:%s\nP:%s\nV:%s\nA:x86_64\nS:%d\nI:1\nT:test\nU:https://example.org\nL:MIT\n\n",
			p.Checksum, p.Name, p.Version, len(p.Bytes))
	}
	body := gzipMember(tarOf(true, [2]string{"DESCRIPTION", "test"}, [2]string{"APKINDEX", idx.String()}))
	sigName, sig := k.sign(body, sha256Sig)
	sigMember := gzipMember(tarOf(false, [2]string{sigName, string(sig)}))
	return append(sigMember, body...)
}

// Repo is an in-memory upstream mirror served over HTTP.
type Repo struct {
	mu    sync.Mutex
	files map[string][]byte
	Hits  map[string]int
}

// NewRepo returns an empty Repo.
func NewRepo() *Repo { return &Repo{files: map[string][]byte{}, Hits: map[string]int{}} }

// Set stores a file at a path such as "v3.20/main/x86_64/APKINDEX.tar.gz".
func (r *Repo) Set(path string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[path] = data
}

// Delete removes a file.
func (r *Repo) Delete(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, path)
}

// ServeHTTP implements http.Handler.
func (r *Repo) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/")
	r.mu.Lock()
	data, ok := r.files[path]
	r.Hits[path]++
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(data)
}

// Hit returns how many times path was requested.
func (r *Repo) Hit(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Hits[path]
}

// Get returns the current content of path (nil if absent).
func (r *Repo) Get(path string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files[path]
}
