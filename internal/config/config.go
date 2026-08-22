// Package config loads, defaults and validates the logstat YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

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

// Redis holds the connection parameters of a single (possibly remote) Redis
// instance plus the expiry applied to the keys logstat writes.
type Redis struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	DB       int    `yaml:"db"`
	Password string `yaml:"password"`
	// TTL is the key expiry in seconds, 0 meaning no expiry. It is refreshed on
	// every flush, so it measures how long the keys outlive the daemon rather
	// than how long they exist.
	TTL int `yaml:"ttl"`
}

// Addr returns the host:port string accepted by go-redis.
func (r Redis) Addr() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

// TTLDuration returns the key expiry as a duration, 0 meaning no expiry.
func (r Redis) TTLDuration() time.Duration {
	return time.Duration(r.TTL) * time.Second
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

// Metrics configures the Prometheus exporter. It is off by default: an upgrade
// must not open a listening socket where none was expected, and two instances
// on one host would otherwise fight over the same port.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	Path    string `yaml:"path"`
}

// Config is the full daemon configuration.
type Config struct {
	LogPath string   `yaml:"log_path"`
	Actions []string `yaml:"actions"`
	// CaseSensitive selects how the actions are matched against a log line:
	// byte for byte (the default) or with both sides lowercased first.
	CaseSensitive bool    `yaml:"case_sensitive"`
	FlushInterval int     `yaml:"flush_interval"`
	Poll          bool    `yaml:"poll"`
	HeartbeatKey  bool    `yaml:"heartbeat_key"`
	LockFile      string  `yaml:"lock_file"`
	Redis         Redis   `yaml:"redis"`
	Logging       Logging `yaml:"logging"`
	Reset         Reset   `yaml:"reset"`
	Metrics       Metrics `yaml:"metrics"`
}

// Default returns the configuration used when the YAML file omits fields.
func Default() Config {
	return Config{
		LogPath:       "/var/log/app.log",
		Actions:       []string{"get-number", "get-sms", "getNumber", "getStatus"},
		CaseSensitive: true,
		FlushInterval: 10,
		Poll:          false,
		HeartbeatKey:  true,
		LockFile:      "/run/logstat/logstat.lock",
		Redis: Redis{
			Host: "127.0.0.1",
			Port: 6379,
			DB:   0,
			TTL:  86400, // one day
		},
		Logging: Logging{
			Level:  "info",
			Output: OutputJournald,
		},
		Reset: Reset{
			Enabled:  true,
			Schedule: "0 0 * * *",
		},
		Metrics: Metrics{
			Enabled: false,
			Listen:  "127.0.0.1:9843",
			Path:    "/metrics",
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
// It returns warnings for suspicious but legal setups and an error for fatal
// ones. Every check runs: a config with three typos reports all three at once
// instead of turning the fix into a restart-per-typo loop.
//
// It also normalises what it can, which today means dropping duplicate actions.
// That makes the call idempotent in its verdict but not in its warnings: the
// second run has no duplicates left to complain about. Load calls it once, and
// the daemon runs on the normalised config.
func (c *Config) Validate() ([]string, error) {
	var warnings []string
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if strings.TrimSpace(c.LogPath) == "" {
		fail("log_path must not be empty")
	}

	if len(c.Actions) == 0 {
		fail("actions must not be empty")
	}
	seen := make(map[string]bool, len(c.Actions))
	uniq := make([]string, 0, len(c.Actions))
	for i, a := range c.Actions {
		if a == "" {
			fail("actions[%d] must not be empty", i)
			continue
		}
		if seen[a] {
			warnings = append(warnings, fmt.Sprintf("action %q is listed more than once, duplicates ignored", a))
			continue
		}
		seen[a] = true
		uniq = append(uniq, a)
	}
	c.Actions = uniq
	warnings = append(warnings, Overlaps(c.Actions, c.CaseSensitive)...)

	if c.FlushInterval <= 0 {
		fail("flush_interval must be > 0, got %d", c.FlushInterval)
	}

	if strings.TrimSpace(c.LockFile) == "" {
		fail("lock_file must not be empty")
	}

	if strings.TrimSpace(c.Redis.Host) == "" {
		fail("redis.host must not be empty")
	}
	if c.Redis.Port < 1 || c.Redis.Port > 65535 {
		fail("redis.port must be in 1..65535, got %d", c.Redis.Port)
	}
	if c.Redis.DB < 0 {
		fail("redis.db must be >= 0, got %d", c.Redis.DB)
	}
	if c.Redis.TTL < 0 {
		fail("redis.ttl must be >= 0 seconds, got %d", c.Redis.TTL)
	}
	// A TTL shorter than the flush interval would let the keys expire between two
	// flushes even though the daemon is alive and counting.
	if c.Redis.TTL > 0 && c.FlushInterval > 0 && c.Redis.TTL <= c.FlushInterval {
		warnings = append(warnings, fmt.Sprintf(
			"redis.ttl (%ds) is not longer than flush_interval (%ds): keys may expire between two flushes and lose their total",
			c.Redis.TTL, c.FlushInterval))
	}

	if !slices.Contains(Levels, c.Logging.Level) {
		fail("logging.level must be one of %s, got %q", strings.Join(Levels, "/"), c.Logging.Level)
	}
	switch c.Logging.Output {
	case OutputJournald:
	case OutputFile:
		if strings.TrimSpace(c.Logging.File) == "" {
			fail(`logging.file must not be empty when logging.output is "file"`)
		}
	default:
		fail(`logging.output must be %q or %q, got %q`, OutputJournald, OutputFile, c.Logging.Output)
	}

	// The schedule is validated regardless of reset.enabled so that a typo is not
	// discovered months later, when someone flips the flag on.
	if _, err := ParseSchedule(c.Reset.Schedule); err != nil {
		errs = append(errs, err)
	}
	// Same reasoning for the exporter: checked whether or not it is enabled.
	if err := c.Metrics.validate(); err != nil {
		errs = append(errs, err)
	}

	return warnings, join(errs)
}

// join collapses the collected problems into one error. It keeps them on a
// single line, because this ends up as the value of a log attribute, and keeps
// them unwrappable, so errors.Is still works on any of them.
func join(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return multiError(errs)
	}
}

type multiError []error

func (m multiError) Error() string {
	msgs := make([]string, 0, len(m))
	for _, err := range m {
		msgs = append(msgs, err.Error())
	}
	return strings.Join(msgs, "; ")
}

func (m multiError) Unwrap() []error { return m }

// validate checks the exporter address and path without binding anything.
func (m Metrics) validate() error {
	listen := strings.TrimSpace(m.Listen)
	if listen == "" {
		return errors.New("metrics.listen must not be empty")
	}
	// An empty host is legal and means every interface; the port is not.
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("metrics.listen %q must be host:port: %w", m.Listen, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("metrics.listen %q must end in a numeric port", m.Listen)
	}
	// Port 0 would let the kernel pick a random port, which nothing could scrape.
	if n < 1 || n > 65535 {
		return fmt.Errorf("metrics.listen port must be in 1..65535, got %d", n)
	}

	return m.ValidatePath()
}

// ValidatePath checks that the endpoint path is a plain path. It is exported
// because the exporter repeats the check on its own input: the path ends up in
// an http.ServeMux, which reads "{...}" as a wildcard and *panics* on a broken
// one, so an unchecked path would turn a config typo into a crash loop instead
// of a startup error.
func (m Metrics) ValidatePath() error {
	if !strings.HasPrefix(m.Path, "/") {
		return fmt.Errorf("metrics.path must start with a slash, got %q", m.Path)
	}
	if strings.ContainsAny(m.Path, "{}") {
		return fmt.Errorf("metrics.path must not contain { or }, got %q: "+
			"the path is taken literally, not as a routing pattern", m.Path)
	}
	if strings.ContainsFunc(m.Path, unicode.IsSpace) {
		return fmt.Errorf("metrics.path must not contain whitespace, got %q", m.Path)
	}
	// To net/http a trailing slash means a subtree, so "/metrics/" would answer
	// on "/metrics/anything" as well. The root is the one legal exception.
	if m.Path != "/" && strings.HasSuffix(m.Path, "/") {
		return fmt.Errorf("metrics.path must not end with a slash, got %q: "+
			"that would serve the metrics on every path below it", m.Path)
	}
	return nil
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
// The comparison follows the matching mode: with caseSensitive false the case is
// folded away first, so words that overlap only in that mode are reported too.
//
// A pair differing only in case is a special case of that: the two words always
// match together yet keep two separate Redis keys. It gets a warning of its own,
// once per pair rather than once per direction.
func Overlaps(actions []string, caseSensitive bool) []string {
	needles := make([]string, len(actions))
	for i, a := range actions {
		if caseSensitive {
			needles[i] = a
		} else {
			needles[i] = strings.ToLower(a)
		}
	}

	var warnings []string
	for i, a := range actions {
		for j, b := range actions {
			if i == j || a == "" || b == "" {
				continue
			}
			if needles[i] == needles[j] {
				// Only reachable without case sensitivity: exact duplicates are
				// dropped before the check.
				if i < j {
					warnings = append(warnings, fmt.Sprintf(
						"actions %q and %q differ only in case: with case_sensitive: no they match the same lines but keep two separate counters",
						a, b))
				}
				continue
			}
			if strings.Contains(needles[j], needles[i]) {
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
