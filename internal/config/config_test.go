package config

import (
	"strings"
	"testing"
	"time"
)

const validYAML = `
storage:
  endpoint: minio.lan:9000
  bucket: mirror
  access_key_env: S3_ACCESS_KEY
  secret_key_env: S3_SECRET_KEY
repos:
  - name: alpine
    type: apk
    upstream: https://dl-cdn.alpinelinux.org/alpine
    prefix: alpine
    schedule: "0 */3 * * *"
    apk:
      branches: [v3.20]
      repos: [main]
      architectures: [x86_64]
`

func TestParseValidAppliesDefaults(t *testing.T) {
	c, err := Parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.Concurrency != 8 {
		t.Errorf("concurrency = %d, want 8", c.Defaults.Concurrency)
	}
	if c.Defaults.Retention.GracePeriod != 72*time.Hour {
		t.Errorf("grace = %v, want 72h", c.Defaults.Retention.GracePeriod)
	}
	if c.Server.Listen != ":8080" || c.Log.Format != "json" || c.Storage.Region != "us-east-1" {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"unknown field", func(s string) string { return s + "\nbogus: 1\n" }, "bogus"},
		{"bad type", func(s string) string { return strings.Replace(s, "type: apk", "type: zip", 1) }, "must be one of apt, rpm, apk"},
		{"missing section", func(s string) string {
			return strings.Replace(s, "type: apk", "type: apt", 1)
		}, `requires an "apt" section`},
		{"bad upstream", func(s string) string {
			return strings.Replace(s, "https://dl-cdn.alpinelinux.org/alpine", "ftp://x/y", 1)
		}, "http(s) URL"},
		{"traversal prefix", func(s string) string { return strings.Replace(s, "prefix: alpine", "prefix: ../etc", 1) }, "invalid path segment"},
		{"absolute prefix", func(s string) string { return strings.Replace(s, "prefix: alpine", "prefix: /alpine", 1) }, "relative path"},
		{"empty list", func(s string) string { return strings.Replace(s, "branches: [v3.20]", "branches: []", 1) }, "apk.branches must not be empty"},
		{"no bucket", func(s string) string { return strings.Replace(s, "bucket: mirror", "", 1) }, "storage.bucket is required"},
		{"bad name", func(s string) string { return strings.Replace(s, "name: alpine", "name: Alpine_X", 1) }, "name must match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.mutate(validYAML)))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDuplicateNameAndPrefix(t *testing.T) {
	repo := `
  - name: alpine
    type: apk
    upstream: https://example.org/alpine
    prefix: alpine
    schedule: "0 * * * *"
    apk: {branches: [v3.20], repos: [main], architectures: [x86_64]}
`
	_, err := Parse(strings.NewReader(validYAML + repo))
	if err == nil || !strings.Contains(err.Error(), "duplicate name") || !strings.Contains(err.Error(), "duplicate prefix") {
		t.Fatalf("err = %v, want duplicate name and prefix", err)
	}
}

func TestCredentials(t *testing.T) {
	s := Storage{AccessKeyEnv: "COURIER_TEST_AK", SecretKeyEnv: "COURIER_TEST_SK"}
	if _, _, err := s.Credentials(); err == nil {
		t.Fatal("expected error when env unset")
	}
	t.Setenv("COURIER_TEST_AK", "a")
	t.Setenv("COURIER_TEST_SK", "b")
	a, b, err := s.Credentials()
	if err != nil || a != "a" || b != "b" {
		t.Fatalf("got %q %q %v", a, b, err)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	if _, err := Load("../../config.example.yaml"); err != nil {
		t.Fatal(err)
	}
}
