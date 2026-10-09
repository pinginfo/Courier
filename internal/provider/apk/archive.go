package apk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- Alpine index signatures and package checksums are defined with SHA-1.
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Alpine index and package files are concatenations of gzip members:
//
//	APKINDEX.tar.gz = [signature tar] [index tar]
//	pkg.apk         = [signature tar] [control tar] [data tar]
//
// The signature covers, and the index's per-package "C:" checksum is computed
// over, the raw compressed bytes of the member that follows the signature.
// Go's gzip reader does not over-read a bytes.Reader, so member boundaries can
// be recovered from the reader's position.

const (
	maxMemberSize  = 512 << 20 // decompressed size cap per member
	maxControlSize = 8 << 20
	// controlWindow is how much of an .apk is read to find its control member.
	controlWindow = 8 << 20
)

// member is one gzip member of an archive.
type member struct {
	raw   []byte // compressed bytes, including gzip header and trailer
	files []tarFile
}

type tarFile struct {
	name string
	data []byte
}

// splitMembers decodes up to maxMembers gzip members from data. For each member it
// reads at most limit decompressed bytes of tar entries.
func splitMembers(data []byte, maxMembers int, limit int64) ([]member, error) {
	r := bytes.NewReader(data)
	var out []member
	for len(out) < maxMembers && r.Len() > 0 {
		start := len(data) - r.Len()
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("gzip member %d: %w", len(out)+1, err)
		}
		gz.Multistream(false)
		plain, err := io.ReadAll(io.LimitReader(gz, limit+1))
		if err != nil {
			return nil, fmt.Errorf("gzip member %d: %w", len(out)+1, err)
		}
		if int64(len(plain)) > limit {
			return nil, fmt.Errorf("gzip member %d exceeds %d bytes", len(out)+1, limit)
		}
		end := len(data) - r.Len()
		files, err := readTar(plain)
		if err != nil {
			return nil, fmt.Errorf("tar in member %d: %w", len(out)+1, err)
		}
		out = append(out, member{raw: data[start:end], files: files})
	}
	return out, nil
}

// readTar reads all entries of a tar stream. A missing end-of-archive marker
// (as in the signature member) is fine.
func readTar(plain []byte) ([]tarFile, error) {
	tr := tar.NewReader(bytes.NewReader(plain))
	var files []tarFile
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files = append(files, tarFile{name: h.Name, data: b})
	}
}

func (m member) isSignature() bool {
	return len(m.files) > 0 && strings.HasPrefix(m.files[0].name, ".SIGN.")
}

// indexArchive is a decoded APKINDEX.tar.gz.
type indexArchive struct {
	sigName string // e.g. ".SIGN.RSA256.alpine-devel@lists.alpinelinux.org-6165ee59.rsa.pub"
	sig     []byte
	signed  []byte // raw bytes the signature covers
	index   []byte // contents of the APKINDEX entry
}

func parseIndexArchive(data []byte) (*indexArchive, error) {
	ms, err := splitMembers(data, 2, maxMemberSize)
	if err != nil {
		return nil, err
	}
	if len(ms) != 2 || !ms[0].isSignature() {
		return nil, errors.New("APKINDEX.tar.gz is not signed (expected a signature member followed by the index)")
	}
	ia := &indexArchive{sigName: ms[0].files[0].name, sig: ms[0].files[0].data, signed: ms[1].raw}
	for _, f := range ms[1].files {
		if f.name == "APKINDEX" {
			ia.index = f.data
		}
	}
	if ia.index == nil {
		return nil, errors.New("APKINDEX.tar.gz has no APKINDEX entry")
	}
	return ia, nil
}

// keyRing holds trusted RSA public keys by file name.
type keyRing map[string]*rsa.PublicKey

// loadKeyRing reads every PEM public key file in dir.
func loadKeyRing(dir string) (keyRing, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read keys_dir: %w", err)
	}
	kr := keyRing{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		pemBytes, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 G703 -- operator-provided directory
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(pemBytes)
		if block == nil {
			return nil, fmt.Errorf("%s: not PEM", e.Name())
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		rsaPub, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an RSA key", e.Name())
		}
		kr[e.Name()] = rsaPub
	}
	if len(kr) == 0 {
		return nil, fmt.Errorf("no .pub keys found in %s", dir)
	}
	return kr, nil
}

// verify checks the archive signature against the key ring.
func (ia *indexArchive) verify(kr keyRing) error {
	var hash crypto.Hash
	var keyName string
	switch {
	case strings.HasPrefix(ia.sigName, ".SIGN.RSA256."):
		hash, keyName = crypto.SHA256, strings.TrimPrefix(ia.sigName, ".SIGN.RSA256.")
	case strings.HasPrefix(ia.sigName, ".SIGN.RSA."):
		hash, keyName = crypto.SHA1, strings.TrimPrefix(ia.sigName, ".SIGN.RSA.")
	default:
		return fmt.Errorf("unsupported signature type %q", ia.sigName)
	}
	pub, ok := kr[keyName]
	if !ok {
		return fmt.Errorf("index is signed with %q, which is not in keys_dir", keyName)
	}
	var sum []byte
	if hash == crypto.SHA256 {
		s := sha256.Sum256(ia.signed)
		sum = s[:]
	} else {
		s := sha1.Sum(ia.signed) // #nosec G401 -- SHA-1 is mandated by the Alpine format
		sum = s[:]
	}
	if err := rsa.VerifyPKCS1v15(pub, hash, sum, ia.sig); err != nil {
		return fmt.Errorf("signature verification failed for key %q: %w", keyName, err)
	}
	return nil
}

// controlChecksum returns the "Q1"-prefixed checksum an APKINDEX "C:" field
// holds for the .apk whose first bytes are head: the SHA-1 of the compressed
// control member.
func controlChecksum(head []byte) (string, error) {
	ms, err := splitMembers(head, 2, maxControlSize)
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if m.isSignature() {
			continue
		}
		sum := sha1.Sum(m.raw) // #nosec G401 -- SHA-1 is mandated by the Alpine format
		return "Q1" + base64.StdEncoding.EncodeToString(sum[:]), nil
	}
	return "", errors.New("no control segment found")
}

// verifyPackage is a provider.Package.Verify: it checks the control-segment
// checksum of the downloaded .apk against the index's C: field.
func verifyPackage(wantC string) func(string) error {
	return func(path string) error {
		f, err := os.Open(path) // #nosec G304 G703 -- engine temp file
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		head, err := io.ReadAll(io.LimitReader(f, controlWindow))
		if err != nil {
			return err
		}
		got, err := controlChecksum(head)
		if err != nil {
			return err
		}
		if got != wantC {
			return fmt.Errorf("control checksum %s does not match index %s", got, wantC)
		}
		return nil
	}
}
