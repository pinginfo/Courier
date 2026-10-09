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
	"strings"
	"time"

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
	Concurrency int       `yaml:"concurrency"`
	RateLimit   string    `yaml:"rate_limit"`
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

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

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
		if strings.TrimSpace(r.Schedule) == "" {
			add("%s: schedule is required", where)
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
		}
	}
	switch r.Type {
	case TypeAPT:
		need("apt", r.APT != nil, nil)
		if r.APT != nil {
			need("apt", true, map[string][]string{"suites": r.APT.Suites, "components": r.APT.Components, "architectures": r.APT.Architectures})
		}
	case TypeRPM:
		need("rpm", r.RPM != nil, nil)
		if r.RPM != nil {
			need("rpm", true, map[string][]string{"releases": r.RPM.Releases, "repos": r.RPM.Repos, "architectures": r.RPM.Architectures})
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
