// Package config loads, defaults and validates the logstat YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Logging output modes.
const (
	OutputJournald = "journald"
	OutputFile     = "file"
)

// Levels accepted by logging.level, in increasing order of severity.
var Levels = []string{"debug", "info", "warn", "error"}

// Redis holds the connection parameters of a single (possibly remote) Redis instance.
type Redis struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	DB       int    `yaml:"db"`
	Password string `yaml:"password"`
}

// Addr returns the host:port string accepted by go-redis.
func (r Redis) Addr() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

// Logging configures the daemon's own log (not the watched log file).
type Logging struct {
	Level  string `yaml:"level"`
	Output string `yaml:"output"`
	File   string `yaml:"file"`
}

// Reset configures the optional self-reset of the counters.
type Reset struct {
	Enabled  bool   `yaml:"enabled"`
	Schedule string `yaml:"schedule"`
}

// Config is the full daemon configuration.
type Config struct {
	LogPath       string   `yaml:"log_path"`
	Actions       []string `yaml:"actions"`
	FlushInterval int      `yaml:"flush_interval"`
	Poll          bool     `yaml:"poll"`
	HeartbeatKey  bool     `yaml:"heartbeat_key"`
	LockFile      string   `yaml:"lock_file"`
	Redis         Redis    `yaml:"redis"`
	Logging       Logging  `yaml:"logging"`
	Reset         Reset    `yaml:"reset"`
}

// Default returns the configuration used when the YAML file omits fields.
func Default() Config {
	return Config{
		LogPath:       "/var/log/app.log",
		Actions:       []string{"get-number", "get-sms", "getNumber", "getStatus"},
		FlushInterval: 10,
		Poll:          false,
		HeartbeatKey:  true,
		LockFile:      "/run/logstat/logstat.lock",
		Redis: Redis{
			Host: "127.0.0.1",
			Port: 6379,
			DB:   0,
		},
		Logging: Logging{
			Level:  "info",
			Output: OutputJournald,
		},
		Reset: Reset{
			Enabled:  true,
			Schedule: "0 0 * * *",
		},
	}
}

// Parse decodes YAML on top of the built-in defaults. Absent fields keep their
// default value; unknown fields are rejected so typos do not pass silently.
func Parse(data []byte) (*Config, error) {
	cfg := Default()

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		// An empty document is not an error: everything falls back to defaults.
		if errors.Is(err, io.EOF) {
			return &cfg, nil
		}
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return &cfg, nil
}

// Load reads, parses and validates the configuration file at path.
// Non-fatal remarks (e.g. prefix overlaps between actions) are returned as warnings.
func Load(path string) (cfg *Config, warnings []string, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path comes from the operator via --config
	if err != nil {
		return nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err = Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("config %s: %w", path, err)
	}
	warnings, err = cfg.Validate()
	if err != nil {
		return nil, warnings, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, warnings, nil
}

// Validate checks the configuration without touching the filesystem.
// It returns warnings for suspicious but legal setups and an error for fatal ones.
func (c *Config) Validate() ([]string, error) {
	var warnings []string

	if strings.TrimSpace(c.LogPath) == "" {
		return warnings, errors.New("log_path must not be empty")
	}

	if len(c.Actions) == 0 {
		return warnings, errors.New("actions must not be empty")
	}
	seen := make(map[string]bool, len(c.Actions))
	uniq := make([]string, 0, len(c.Actions))
	for i, a := range c.Actions {
		if a == "" {
			return warnings, fmt.Errorf("actions[%d] must not be empty", i)
		}
		if seen[a] {
			warnings = append(warnings, fmt.Sprintf("action %q is listed more than once, duplicates ignored", a))
			continue
		}
		seen[a] = true
		uniq = append(uniq, a)
	}
	c.Actions = uniq
	warnings = append(warnings, Overlaps(c.Actions)...)

	if c.FlushInterval <= 0 {
		return warnings, fmt.Errorf("flush_interval must be > 0, got %d", c.FlushInterval)
	}

	if strings.TrimSpace(c.LockFile) == "" {
		return warnings, errors.New("lock_file must not be empty")
	}

	if strings.TrimSpace(c.Redis.Host) == "" {
		return warnings, errors.New("redis.host must not be empty")
	}
	if c.Redis.Port < 1 || c.Redis.Port > 65535 {
		return warnings, fmt.Errorf("redis.port must be in 1..65535, got %d", c.Redis.Port)
	}
	if c.Redis.DB < 0 {
		return warnings, fmt.Errorf("redis.db must be >= 0, got %d", c.Redis.DB)
	}

	if !slices.Contains(Levels, c.Logging.Level) {
		return warnings, fmt.Errorf("logging.level must be one of %s, got %q",
			strings.Join(Levels, "/"), c.Logging.Level)
	}
	switch c.Logging.Output {
	case OutputJournald:
	case OutputFile:
		if strings.TrimSpace(c.Logging.File) == "" {
			return warnings, errors.New(`logging.file must not be empty when logging.output is "file"`)
		}
	default:
		return warnings, fmt.Errorf(`logging.output must be %q or %q, got %q`, OutputJournald, OutputFile, c.Logging.Output)
	}

	// The schedule is validated regardless of reset.enabled so that a typo is not
	// discovered months later, when someone flips the flag on.
	if _, err := ParseSchedule(c.Reset.Schedule); err != nil {
		return warnings, err
	}

	return warnings, nil
}

// ParseSchedule parses a strictly standard 5-field cron expression
// (minute hour day-of-month month day-of-week).
func ParseSchedule(expr string) (cron.Schedule, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, errors.New("reset.schedule must not be empty")
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("reset.schedule %q is not a valid 5-field cron expression: %w", expr, err)
	}
	return sched, nil
}

// Overlaps reports actions that are a substring of another action. Substring
// matching makes such a pair count the same line twice, which is rarely intended.
func Overlaps(actions []string) []string {
	var warnings []string
	for i, a := range actions {
		for j, b := range actions {
			if i == j || a == "" || b == "" {
				continue
			}
			if strings.Contains(b, a) {
				warnings = append(warnings, fmt.Sprintf(
					"action %q is a substring of action %q: every line matching %q also increments %q",
					a, b, b, a))
			}
		}
	}
	return warnings
}

// CheckPaths performs the filesystem-touching part of the validation: it makes
// sure the lock file directory exists and that the daemon log file is writable.
func (c *Config) CheckPaths() error {
	if err := ensureDir(filepath.Dir(c.LockFile)); err != nil {
		return fmt.Errorf("lock_file %s: %w", c.LockFile, err)
	}
	if c.Logging.Output == OutputFile {
		if err := ensureDir(filepath.Dir(c.Logging.File)); err != nil {
			return fmt.Errorf("logging.file %s: %w", c.Logging.File, err)
		}
		f, err := os.OpenFile(c.Logging.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // operator-provided path
		if err != nil {
			return fmt.Errorf("logging.file %s is not writable: %w", c.Logging.File, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("logging.file %s: %w", c.Logging.File, err)
		}
	}
	return nil
}

func ensureDir(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create directory %s: %w", dir, err)
	}
	return nil
}
