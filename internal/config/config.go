// Package config loads and validates the Courier configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Repo types supported by Courier.
const (
	TypeAPT = "apt"
	TypeRPM = "rpm"
	TypeAPK = "apk"
)

// Config is the root of the configuration file.
type Config struct {
	Storage  Storage  `yaml:"storage"`
	Defaults Defaults `yaml:"defaults"`
	Repos    []Repo   `yaml:"repos"`
	Server   Server   `yaml:"server"`
	Log      Log      `yaml:"log"`
}

// Storage describes the S3 (MinIO) target. Credentials are read from the
// environment variables named by AccessKeyEnv / SecretKeyEnv, never inline.
type Storage struct {
	Endpoint     string `yaml:"endpoint"`
	Bucket       string `yaml:"bucket"`
	Region       string `yaml:"region"`
	UseSSL       bool   `yaml:"use_ssl"`
	PathStyle    bool   `yaml:"path_style"`
	CAFile       string `yaml:"ca_file"`
	AccessKeyEnv string `yaml:"access_key_env"`
	SecretKeyEnv string `yaml:"secret_key_env"`
}

// Defaults apply to every repo unless overridden.
type Defaults struct {
	Concurrency int    `yaml:"concurrency"`
	RateLimit   string `yaml:"rate_limit"`
	// MaxTempSize bounds the temporary disk space used by in-flight package
	// downloads, for example "4GB" (the default). A package larger than the
	// budget is still downloaded, alone.
	MaxTempSize string    `yaml:"max_temp_size"`
	Retention   Retention `yaml:"retention"`
}

// Retention controls garbage collection of unreferenced objects.
type Retention struct {
	GracePeriod time.Duration `yaml:"grace_period"`
}

// Repo is one mirrored repository.
type Repo struct {
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`
	Upstream   string `yaml:"upstream"`
	Prefix     string `yaml:"prefix"`
	Schedule   string `yaml:"schedule"`
	GPGKeyring string `yaml:"gpg_keyring"`

	APT *APT `yaml:"apt"`
	RPM *RPM `yaml:"rpm"`
	APK *APK `yaml:"apk"`
}

// APT filters for a Debian/Ubuntu repository.
type APT struct {
	Suites        []string `yaml:"suites"`
	Components    []string `yaml:"components"`
	Architectures []string `yaml:"architectures"`
	// Languages selects which i18n Translation-<lang> files are mirrored.
	// Defaults to ["en"].
	Languages []string `yaml:"languages"`
	// Contents also mirrors the (very large) Contents-<arch> indexes used by
	// apt-file. Off by default.
	Contents bool `yaml:"contents"`
}

// RPM filters for a Fedora-style repository.
type RPM struct {
	Releases      []string `yaml:"releases"`
	Repos         []string `yaml:"repos"`
	Architectures []string `yaml:"architectures"`
}

// APK filters for an Alpine repository.
type APK struct {
	Branches      []string `yaml:"branches"`
	Repos         []string `yaml:"repos"`
	Architectures []string `yaml:"architectures"`
	// KeysDir optionally holds trusted Alpine *.rsa.pub keys. When set, the
	// APKINDEX signature is verified before anything is mirrored; when empty,
	// the index is mirrored as is (clients still verify it on apk update).
	KeysDir string `yaml:"keys_dir"`
}

// Server holds HTTP server settings.
type Server struct {
	Listen string `yaml:"listen"`
}

// Log holds logging settings.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	releaseRe = regexp.MustCompile(`^([0-9]+|rawhide)$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	rateRe    = regexp.MustCompile(`^(\d+)\s*([KMG]?)B/s$`)
)

// ParseRate converts a rate such as "50MB/s" into bytes per second. An empty
// string or "0" means unlimited (0).
func ParseRate(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	m := rateRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("rate %q must look like 50MB/s (units B, KB, MB, GB)", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	switch m[2] {
	case "K":
		n <<= 10
	case "M":
		n <<= 20
	case "G":
		n <<= 30
	}
	return n, nil
}

// Load reads, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path) // #nosec G304 -- path is an operator-provided flag
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()
	return Parse(f)
}

// Parse decodes, defaults and validates a config from r. Unknown fields are errors.
func Parse(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Defaults.Concurrency == 0 {
		c.Defaults.Concurrency = 8
	}
	if c.Defaults.Retention.GracePeriod == 0 {
		c.Defaults.Retention.GracePeriod = 72 * time.Hour
	}
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
	if c.Storage.Region == "" {
		c.Storage.Region = "us-east-1"
	}
}

// Validate checks the whole config and returns every problem found.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Storage.Endpoint == "" {
		add("storage.endpoint is required")
	}
	if c.Storage.Bucket == "" {
		add("storage.bucket is required")
	}
	if c.Storage.AccessKeyEnv == "" || c.Storage.SecretKeyEnv == "" {
		add("storage.access_key_env and storage.secret_key_env are required")
	}
	if c.Defaults.Concurrency < 0 {
		add("defaults.concurrency must be positive")
	}
	if _, err := ParseRate(c.Defaults.RateLimit); err != nil {
		add("defaults.rate_limit: %v", err)
	}
	if _, err := ParseSize(c.Defaults.MaxTempSize); err != nil {
		add("defaults.max_temp_size: %v", err)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level %q must be one of debug, info, warn, error", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format %q must be json or text", c.Log.Format)
	}

	if len(c.Repos) == 0 {
		add("at least one repo is required")
	}
	names := map[string]bool{}
	prefixes := map[string]bool{}
	for i, r := range c.Repos {
		where := fmt.Sprintf("repos[%d]", i)
		if r.Name != "" {
			where = "repo " + r.Name
		}
		if !nameRe.MatchString(r.Name) {
			add("%s: name must match %s", where, nameRe)
		}
		if names[r.Name] {
			add("%s: duplicate name", where)
		}
		names[r.Name] = true

		if err := validateUpstream(r.Upstream); err != nil {
			add("%s: upstream: %v", where, err)
		}
		if err := validatePrefix(r.Prefix); err != nil {
			add("%s: prefix: %v", where, err)
		}
		if prefixes[r.Prefix] {
			add("%s: duplicate prefix %q", where, r.Prefix)
		}
		prefixes[r.Prefix] = true
		if _, err := cron.ParseStandard(r.Schedule); err != nil {
			add("%s: schedule %q: %v", where, r.Schedule, err)
		}
		errs = append(errs, validateTypeSection(where, r)...)
	}
	return errors.Join(errs...)
}

func validateTypeSection(where string, r Repo) []error {
	var errs []error
	need := func(section string, present bool, lists map[string][]string) {
		if !present {
			errs = append(errs, fmt.Errorf("%s: type %s requires an %q section", where, r.Type, section))
			return
		}
		for k, v := range lists {
			if len(v) == 0 {
				errs = append(errs, fmt.Errorf("%s: %s.%s must not be empty", where, section, k))
			}
			// These values become URL and object-key path segments.
			for _, s := range v {
				if !segmentRe.MatchString(s) {
					errs = append(errs, fmt.Errorf("%s: %s.%s: %q is not a valid path segment", where, section, k, s))
				}
			}
		}
	}
	switch r.Type {
	case TypeAPT:
		need("apt", r.APT != nil, nil)
		if r.APT != nil {
			need("apt", true, map[string][]string{"suites": r.APT.Suites, "components": r.APT.Components, "architectures": r.APT.Architectures})
			for _, l := range r.APT.Languages {
				if !segmentRe.MatchString(l) {
					errs = append(errs, fmt.Errorf("%s: apt.languages: %q is not a valid language code", where, l))
				}
			}
		}
	case TypeRPM:
		need("rpm", r.RPM != nil, nil)
		if r.RPM != nil {
			need("rpm", true, map[string][]string{"releases": r.RPM.Releases, "repos": r.RPM.Repos, "architectures": r.RPM.Architectures})
			for _, rel := range r.RPM.Releases {
				if !releaseRe.MatchString(rel) {
					errs = append(errs, fmt.Errorf("%s: rpm.releases: %q must be a release number or \"rawhide\"", where, rel))
				}
				if rel == "rawhide" {
					for _, name := range r.RPM.Repos {
						if name != "releases" {
							errs = append(errs, fmt.Errorf("%s: rpm.repos: rawhide only has the \"releases\" repo, not %q", where, name))
						}
					}
				}
			}
			for _, name := range r.RPM.Repos {
				switch name {
				case "releases", "updates", "updates-testing":
				default:
					errs = append(errs, fmt.Errorf("%s: rpm.repos: %q must be one of releases, updates, updates-testing", where, name))
				}
			}
		}
	case TypeAPK:
		need("apk", r.APK != nil, nil)
		if r.APK != nil {
			need("apk", true, map[string][]string{"branches": r.APK.Branches, "repos": r.APK.Repos, "architectures": r.APK.Architectures})
		}
	default:
		errs = append(errs, fmt.Errorf("%s: type %q must be one of apt, rpm, apk", where, r.Type))
	}
	return errs
}

func validateUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must be an http(s) URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}

// validatePrefix rejects empty, absolute or traversing bucket prefixes, since
// object keys are built from it.
func validatePrefix(p string) error {
	if p == "" {
		return errors.New("is required")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return fmt.Errorf("%q must be a relative path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%q contains an invalid path segment", p)
		}
	}
	return nil
}

// Credentials resolves the S3 credentials from the environment.
func (s Storage) Credentials() (accessKey, secretKey string, err error) {
	accessKey, secretKey = os.Getenv(s.AccessKeyEnv), os.Getenv(s.SecretKeyEnv)
	if accessKey == "" || secretKey == "" {
		return "", "", fmt.Errorf("environment variables %s and %s must be set", s.AccessKeyEnv, s.SecretKeyEnv)
	}
	return accessKey, secretKey, nil
}

var sizeRe = regexp.MustCompile(`^(\d+)\s*([KMG]?)B$`)

// DefaultMaxTempBytes is the in-flight download budget when none is configured.
const DefaultMaxTempBytes = 4 << 30

// ParseSize converts a size such as "4GB" into bytes. An empty string yields 0.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("size %q must look like 4GB (units B, KB, MB, GB)", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	switch m[2] {
	case "K":
		n <<= 10
	case "M":
		n <<= 20
	case "G":
		n <<= 30
	}
	return n, nil
}

// MaxTempBytes returns the configured in-flight download budget in bytes.
func (d Defaults) MaxTempBytes() int64 {
	if n, err := ParseSize(d.MaxTempSize); err == nil && n > 0 {
		return n
	}
	return DefaultMaxTempBytes
}
