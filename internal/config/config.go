// Package config loads the repohub server configuration from a versioned YAML
// file plus environment-variable overrides. Nothing deployment-specific is
// hard-coded: every path, host and limit can be set here.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CurrentVersion is the config file format version this binary understands.
const CurrentVersion = 1

type Config struct {
	Version int `yaml:"version"`
	// Listen is the address of the public HTTP listener (Git + API).
	Listen string `yaml:"listen"`
	// PublicURL is how clients reach this server, used to print clone URLs.
	PublicURL string `yaml:"public_url"`
	// DataDir holds bare repositories, the SQLite database and runtime files.
	DataDir string `yaml:"data_dir"`
	// SecretKeyEnv names the environment variable holding the 32-byte master
	// key (base64 or hex) used to encrypt platform credentials.
	SecretKeyEnv string `yaml:"secret_key_env"`
	// SecretKeyFile is an alternative to SecretKeyEnv.
	SecretKeyFile string `yaml:"secret_key_file"`
	// AdminTokenEnv names the environment variable holding the bootstrap
	// admin token. Additional tokens are managed through the API.
	AdminTokenEnv string `yaml:"admin_token_env"`

	Git    GitConfig    `yaml:"git"`
	Sync   SyncConfig   `yaml:"sync"`
	Policy PolicyConfig `yaml:"policy"`
}

type GitConfig struct {
	// Binary is the git executable (default "git").
	Binary string `yaml:"binary"`
	// SelfPath is the repohub executable invoked by Git hooks and GIT_ASKPASS.
	// Defaults to the running executable.
	SelfPath string `yaml:"self_path"`
}

type SyncConfig struct {
	// Workers caps concurrent remote operations across all repositories.
	Workers int `yaml:"workers"`
	// PatrolInterval is how often every repository is fully re-checked.
	PatrolInterval Duration `yaml:"patrol_interval"`
	// PushTimeout bounds a single remote sync (ls-remote + push + verify).
	PushTimeout Duration `yaml:"push_timeout"`
	// PostReceiveWait is how long `git push` waits for remote results before
	// returning and letting the sync continue in the background.
	PostReceiveWait Duration `yaml:"post_receive_wait"`
	// BackupRetention is how long refs/backup/* entries are kept.
	BackupRetention Duration `yaml:"backup_retention"`
	// RetryBase / RetryMax bound the exponential backoff for failed remotes.
	RetryBase Duration `yaml:"retry_base"`
	RetryMax  Duration `yaml:"retry_max"`
}

type PolicyConfig struct {
	// MaxFileSize is a hard cap on any single blob pushed to the hub. The
	// effective limit per repository is the smallest of this value and the
	// limits of all remotes the repository mirrors to (e.g. GitHub's 100 MiB).
	// Zero means "derive from remotes only".
	MaxFileSize ByteSize `yaml:"max_file_size"`
}

// Default returns a configuration with all defaults applied.
func Default() *Config {
	return &Config{
		Version:       CurrentVersion,
		Listen:        ":8080",
		PublicURL:     "http://localhost:8080",
		DataDir:       "./data",
		SecretKeyEnv:  "REPOHUB_SECRET_KEY",
		AdminTokenEnv: "REPOHUB_ADMIN_TOKEN",
		Git:           GitConfig{Binary: "git"},
		Sync: SyncConfig{
			Workers:         4,
			PatrolInterval:  Duration(10 * time.Minute),
			PushTimeout:     Duration(10 * time.Minute),
			PostReceiveWait: Duration(30 * time.Second),
			BackupRetention: Duration(30 * 24 * time.Hour),
			RetryBase:       Duration(30 * time.Second),
			RetryMax:        Duration(time.Hour),
		},
	}
}

// Load reads the YAML file at path (optional; empty means defaults only) and
// then applies REPOHUB_* environment overrides.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		cfg.Version = 0 // must come from the file
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		if cfg.Version == 0 {
			return nil, fmt.Errorf("config %s: missing version (expected %d)", path, CurrentVersion)
		}
		if cfg.Version > CurrentVersion {
			return nil, fmt.Errorf("config %s: version %d is newer than this binary supports (%d)", path, cfg.Version, CurrentVersion)
		}
	}
	cfg.applyEnv()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = abs
	return cfg, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("REPOHUB_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("REPOHUB_PUBLIC_URL"); v != "" {
		c.PublicURL = v
	}
	if v := os.Getenv("REPOHUB_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("REPOHUB_GIT_BINARY"); v != "" {
		c.Git.Binary = v
	}
}

func (c *Config) validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen must not be empty"))
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("data_dir must not be empty"))
	}
	if c.Sync.Workers <= 0 {
		errs = append(errs, errors.New("sync.workers must be positive"))
	}
	if c.Sync.PatrolInterval.D() < time.Minute {
		errs = append(errs, errors.New("sync.patrol_interval must be at least 1m"))
	}
	if c.Sync.PushTimeout.D() <= 0 {
		errs = append(errs, errors.New("sync.push_timeout must be positive"))
	}
	if c.Sync.RetryBase.D() <= 0 || c.Sync.RetryMax.D() < c.Sync.RetryBase.D() {
		errs = append(errs, errors.New("sync.retry_base must be positive and <= sync.retry_max"))
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	return errors.Join(errs...)
}

// Duration is a time.Duration that unmarshals from strings like "30s",
// "10m" or "30d".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	parsed, err := ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// ParseDuration extends time.ParseDuration with a "d" (24h) suffix.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

// ByteSize unmarshals from "100MiB", "50MB", "1GiB" or a plain byte count.
type ByteSize int64

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return err
	}
	*b = ByteSize(v)
	return nil
}

// ParseByteSize parses human-readable sizes. KiB/MiB/GiB are powers of 1024,
// KB/MB/GB powers of 1000.
func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			f, err := strconv.ParseFloat(num, 64)
			if err != nil || f < 0 {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n, nil
}

// Paths derived from DataDir.
func (c *Config) ReposDir() string { return filepath.Join(c.DataDir, "repos") }
func (c *Config) HooksDir() string { return filepath.Join(c.DataDir, "hooks") }
func (c *Config) BinDir() string   { return filepath.Join(c.DataDir, "bin") }
func (c *Config) RunDir() string   { return filepath.Join(c.DataDir, "run") }
func (c *Config) TmpDir() string   { return filepath.Join(c.DataDir, "tmp") }
func (c *Config) HomeDir() string  { return filepath.Join(c.DataDir, "home") }
func (c *Config) SSHDir() string   { return filepath.Join(c.DataDir, "ssh") }
func (c *Config) TrashDir() string { return filepath.Join(c.DataDir, "trash") }
func (c *Config) DBPath() string   { return filepath.Join(c.DataDir, "repohub.db") }
