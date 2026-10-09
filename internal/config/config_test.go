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

func TestParseRate(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false}, {"0", 0, false}, {"500B/s", 500, false}, {"10KB/s", 10 << 10, false},
		{"50MB/s", 50 << 20, false}, {"1GB/s", 1 << 30, false}, {"fast", 0, true}, {"5Mbit", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseRate(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseRate(%q) = %d, %v", tt.in, got, err)
		}
	}
}

func TestSchedulePathSegmentsAndRate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"bad cron", func(s string) string { return strings.Replace(s, `"0 */3 * * *"`, `"every day"`, 1) }, "schedule"},
		{"traversal in branch", func(s string) string { return strings.Replace(s, "[v3.20]", "[../x]", 1) }, "not a valid path segment"},
		{"slash in arch", func(s string) string { return strings.Replace(s, "[x86_64]", "[a/b]", 1) }, "not a valid path segment"},
		{"bad rate", func(s string) string {
			return strings.Replace(s, "repos:\n", "defaults:\n  rate_limit: lots\nrepos:\n", 1)
		}, "defaults.rate_limit"},
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

const fedoraYAML = `
storage:
  endpoint: minio.lan:9000
  bucket: mirror
  access_key_env: S3_ACCESS_KEY
  secret_key_env: S3_SECRET_KEY
repos:
  - name: fedora
    type: rpm
    upstream: https://dl.fedoraproject.org/pub/fedora/linux
    prefix: fedora
    schedule: "30 */6 * * *"
    rpm:
      releases: ["43"]
      repos: [releases, updates]
      architectures: [x86_64]
`

func TestRPMValidation(t *testing.T) {
	if _, err := Parse(strings.NewReader(fedoraYAML)); err != nil {
		t.Fatalf("valid fedora config rejected: %v", err)
	}
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"unknown repo alias", func(s string) string { return strings.Replace(s, "[releases, updates]", "[releases, modular]", 1) }, "must be one of releases, updates, updates-testing"},
		{"bad release", func(s string) string { return strings.Replace(s, `["43"]`, `["f43"]`, 1) }, "release number"},
		{"rawhide with updates", func(s string) string { return strings.Replace(s, `["43"]`, `["rawhide"]`, 1) }, "rawhide only has"},
		{"traversal arch", func(s string) string { return strings.Replace(s, "[x86_64]", "[../x]", 1) }, "not a valid path segment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.mutate(fedoraYAML)))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
	if _, err := Parse(strings.NewReader(strings.Replace(strings.Replace(fedoraYAML, `["43"]`, `["rawhide"]`, 1), "[releases, updates]", "[releases]", 1))); err != nil {
		t.Fatalf("rawhide with releases only must be valid: %v", err)
	}
}

func TestParseSizeAndTempBudget(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "1B": 1, "4KB": 4 << 10, "512MB": 512 << 20, "4GB": 4 << 30} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"4", "big", "4TB", "-1GB", "4 GiB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) accepted", bad)
		}
	}
	if got := (Defaults{}).MaxTempBytes(); got != DefaultMaxTempBytes {
		t.Errorf("default budget = %d", got)
	}
	if got := (Defaults{MaxTempSize: "2GB"}).MaxTempBytes(); got != 2<<30 {
		t.Errorf("configured budget = %d", got)
	}
	y := strings.Replace(validYAML, "repos:\n", "defaults:\n  max_temp_size: lots\nrepos:\n", 1)
	if _, err := Parse(strings.NewReader(y)); err == nil || !strings.Contains(err.Error(), "max_temp_size") {
		t.Errorf("bad max_temp_size accepted: %v", err)
	}
}

func TestRetrySettings(t *testing.T) {
	c, err := Parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.RetryCount() != 5 || c.Defaults.RetryRoundCount() != 3 {
		t.Errorf("defaults: retries=%d rounds=%d, want 5 and 3", c.Defaults.RetryCount(), c.Defaults.RetryRoundCount())
	}
	y := strings.Replace(validYAML, "repos:\n", "defaults:\n  retries: 0\n  retry_rounds: 0\n  retry_backoff: 500ms\nrepos:\n", 1)
	c, err = Parse(strings.NewReader(y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.RetryCount() != 0 || c.Defaults.RetryRoundCount() != 0 || c.Defaults.RetryBackoff != 500*time.Millisecond {
		t.Errorf("an explicit 0 must disable retries, got retries=%d rounds=%d backoff=%v",
			c.Defaults.RetryCount(), c.Defaults.RetryRoundCount(), c.Defaults.RetryBackoff)
	}
	for name, bad := range map[string]string{
		"negative retries":   "  retries: -1\n",
		"too many retries":   "  retries: 500\n",
		"negative rounds":    "  retry_rounds: -2\n",
		"too many rounds":    "  retry_rounds: 99\n",
		"negative backoff":   "  retry_backoff: -1s\n",
		"unparseable":        "  retries: many\n",
		"unknown retry knob": "  retry_forever: true\n",
	} {
		y := strings.Replace(validYAML, "repos:\n", "defaults:\n"+bad+"repos:\n", 1)
		if _, err := Parse(strings.NewReader(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
