// Package storagetest starts an in-process S3 server for tests.
package storagetest

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"courier/internal/config"
)

// Bucket is the bucket created on the fake server.
const Bucket = "mirror"

// Start runs a fake S3 server over TLS (as a real deployment should: over plain
// HTTP the client uses chunked payload signing, which this fake does not decode
// for multipart uploads) and returns a Storage config that points at it, with a
// CA file for its certificate and credentials set in the test environment.
func Start(t testing.TB) config.Storage {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(Bucket); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)

	caFile := filepath.Join(t.TempDir(), "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COURIER_FAKE_AK", "test")
	t.Setenv("COURIER_FAKE_SK", "testtest")
	return config.Storage{ // #nosec G101 -- test environment variable names, not credentials
		Endpoint: strings.TrimPrefix(srv.URL, "https://"), Bucket: Bucket, Region: "us-east-1",
		UseSSL: true, PathStyle: true, CAFile: caFile,
		AccessKeyEnv: "COURIER_FAKE_AK", SecretKeyEnv: "COURIER_FAKE_SK",
	}
}
