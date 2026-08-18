package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil): %v", err)
	}
	want := Default()
	if cfg.LogPath != want.LogPath {
		t.Errorf("log_path = %q, want %q", cfg.LogPath, want.LogPath)
	}
	if len(cfg.Actions) != 4 || cfg.Actions[0] != "get-number" || cfg.Actions[3] != "getStatus" {
		t.Errorf("actions = %v, want %v", cfg.Actions, want.Actions)
	}
	if cfg.FlushInterval != 10 {
		t.Errorf("flush_interval = %d, want 10", cfg.FlushInterval)
	}
	if cfg.Poll {
		t.Error("poll = true, want false")
	}
	if !cfg.HeartbeatKey {
		t.Error("heartbeat_key = false, want true by default")
	}
	if cfg.LockFile != "/run/logstat/logstat.lock" {
		t.Errorf("lock_file = %q", cfg.LockFile)
	}
	if cfg.Redis.Host != "127.0.0.1" || cfg.Redis.Port != 6379 || cfg.Redis.DB != 0 || cfg.Redis.Password != "" {
		t.Errorf("redis = %+v", cfg.Redis)
	}
	if cfg.Redis.TTL != 86400 || cfg.Redis.TTLDuration() != 24*time.Hour {
		t.Errorf("redis.ttl = %d (%s), want 86400 (24h)", cfg.Redis.TTL, cfg.Redis.TTLDuration())
	}
	if cfg.Redis.Addr() != "127.0.0.1:6379" {
		t.Errorf("redis addr = %q", cfg.Redis.Addr())
	}
	if cfg.Logging.Level != "info" || cfg.Logging.Output != OutputJournald || cfg.Logging.File != "" {
		t.Errorf("logging = %+v", cfg.Logging)
	}
	if !cfg.Reset.Enabled || cfg.Reset.Schedule != "0 0 * * *" {
		t.Errorf("reset = %+v", cfg.Reset)
	}
	if _, err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestPartialConfigKeepsDefaults(t *testing.T) {
	cfg, err := Parse([]byte("log_path: /tmp/x.log\nflush_interval: 3\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.LogPath != "/tmp/x.log" || cfg.FlushInterval != 3 {
		t.Fatalf("overrides lost: %+v", cfg)
	}
	if len(cfg.Actions) != 4 || cfg.Redis.Port != 6379 || cfg.Reset.Schedule != "0 0 * * *" {
		t.Fatalf("defaults lost: %+v", cfg)
	}
	if !cfg.HeartbeatKey {
		t.Fatalf("heartbeat_key default lost: %+v", cfg)
	}
	if cfg.Redis.TTL != 86400 {
		t.Fatalf("redis.ttl default lost: %+v", cfg)
	}
}

func TestParseFull(t *testing.T) {
	yaml := `
log_path: /var/log/nginx/access.log
actions:
  - alpha
  - beta
flush_interval: 5
poll: true
heartbeat_key: false
lock_file: /run/logstat/nginx/logstat.lock
redis:
  host: 10.0.0.5
  port: 6380
  db: 3
  password: secret
  ttl: 3600
logging:
  level: debug
  output: file
  file: /var/log/logstat/nginx.log
reset:
  enabled: false
  schedule: "*/30 * * * *"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.LogPath != "/var/log/nginx/access.log" || !cfg.Poll || cfg.FlushInterval != 5 {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.HeartbeatKey {
		t.Error("heartbeat_key = true, want the configured false")
	}
	if cfg.Redis.Addr() != "10.0.0.5:6380" || cfg.Redis.DB != 3 || cfg.Redis.Password != "secret" {
		t.Errorf("redis = %+v", cfg.Redis)
	}
	if cfg.Redis.TTLDuration() != time.Hour {
		t.Errorf("redis.ttl = %s, want 1h", cfg.Redis.TTLDuration())
	}
	if cfg.Logging.Level != "debug" || cfg.Logging.Output != OutputFile {
		t.Errorf("logging = %+v", cfg.Logging)
	}
	if cfg.Reset.Enabled || cfg.Reset.Schedule != "*/30 * * * *" {
		t.Errorf("reset = %+v", cfg.Reset)
	}
	if _, err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"broken yaml", "actions: [unclosed\n"},
		{"wrong type", "flush_interval: not-a-number\n"},
		{"wrong bool", "heartbeat_key: yesplease\n"},
		{"unknown field", "flush_intervals: 10\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.yaml)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestValidateFatal(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"empty actions", func(c *Config) { c.Actions = nil }, "actions must not be empty"},
		{"empty action item", func(c *Config) { c.Actions = []string{"a", ""} }, "actions[1]"},
		{"empty log_path", func(c *Config) { c.LogPath = "  " }, "log_path"},
		{"zero flush_interval", func(c *Config) { c.FlushInterval = 0 }, "flush_interval"},
		{"negative flush_interval", func(c *Config) { c.FlushInterval = -1 }, "flush_interval"},
		{"empty lock_file", func(c *Config) { c.LockFile = "" }, "lock_file"},
		{"empty redis host", func(c *Config) { c.Redis.Host = "" }, "redis.host"},
		{"bad redis port", func(c *Config) { c.Redis.Port = 0 }, "redis.port"},
		{"bad redis db", func(c *Config) { c.Redis.DB = -1 }, "redis.db"},
		{"negative redis ttl", func(c *Config) { c.Redis.TTL = -1 }, "redis.ttl"},
		{"bad level", func(c *Config) { c.Logging.Level = "trace" }, "logging.level"},
		{"bad output", func(c *Config) { c.Logging.Output = "syslog" }, "logging.output"},
		{"file output without file", func(c *Config) { c.Logging.Output = OutputFile }, "logging.file"},
		{"bad schedule", func(c *Config) { c.Reset.Schedule = "every midnight" }, "reset.schedule"},
		{"empty schedule", func(c *Config) { c.Reset.Schedule = "" }, "reset.schedule"},
		{"six field schedule", func(c *Config) { c.Reset.Schedule = "0 0 0 * * *" }, "reset.schedule"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			_, err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A TTL at or below the flush interval lets the keys die between two flushes,
// which silently resets the totals of a running daemon.
func TestValidateWarnsOnTTLShorterThanFlushInterval(t *testing.T) {
	cfg := Default()
	cfg.FlushInterval = 10

	for _, ttl := range []int{1, 10} {
		cfg.Redis.TTL = ttl
		warnings, err := cfg.Validate()
		if err != nil {
			t.Fatalf("ttl=%d must be a warning, not an error: %v", ttl, err)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "redis.ttl") {
			t.Fatalf("ttl=%d warnings = %v", ttl, warnings)
		}
	}

	// Comfortably longer, and "no expiry", are both silent.
	for _, ttl := range []int{11, 86400, 0} {
		cfg.Redis.TTL = ttl
		warnings, err := cfg.Validate()
		if err != nil {
			t.Fatalf("ttl=%d: %v", ttl, err)
		}
		if len(warnings) != 0 {
			t.Fatalf("ttl=%d warnings = %v, want none", ttl, warnings)
		}
	}
}

func TestValidateWarnsOnSubstringActions(t *testing.T) {
	cfg := Default()
	cfg.Actions = []string{"getStatus", "getStatusExtended"}
	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("overlapping actions must be a warning, not an error: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"getStatus"`) {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestDefaultActionsDoNotOverlap(t *testing.T) {
	if w := Overlaps(Default().Actions); len(w) != 0 {
		t.Fatalf("default actions must not overlap, got %v", w)
	}
}

func TestValidateWarnsOnDuplicateActions(t *testing.T) {
	cfg := Default()
	cfg.Actions = []string{"a", "b", "a"}
	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(cfg.Actions) != 2 {
		t.Fatalf("duplicates must be dropped, got %v", cfg.Actions)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "more than once") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(path, []byte("actions: [x]\nflush_interval: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}
	if cfg.FlushInterval != 2 || len(cfg.Actions) != 1 {
		t.Errorf("cfg = %+v", cfg)
	}

	if _, _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("Load of a missing file must fail")
	}
}

func TestCheckPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.LockFile = filepath.Join(dir, "run", "logstat.lock")
	cfg.Logging.Output = OutputFile
	cfg.Logging.File = filepath.Join(dir, "logs", "logstat.log")

	if err := cfg.CheckPaths(); err != nil {
		t.Fatalf("CheckPaths: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run")); err != nil {
		t.Errorf("lock directory not created: %v", err)
	}
	if _, err := os.Stat(cfg.Logging.File); err != nil {
		t.Errorf("log file not created: %v", err)
	}
}

func TestCheckPathsUnwritableLogFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.LockFile = filepath.Join(dir, "logstat.lock")
	cfg.Logging.Output = OutputFile
	cfg.Logging.File = filepath.Join(ro, "logstat.log")
	if err := cfg.CheckPaths(); err == nil {
		t.Fatal("expected an error for an unwritable logging.file")
	}
}

func TestParseScheduleNext(t *testing.T) {
	base := time.Date(2026, 8, 18, 15, 7, 30, 0, time.UTC)
	tests := []struct {
		expr string
		want time.Time
	}{
		{"0 0 * * *", time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)},
		{"1 * * * *", time.Date(2026, 8, 18, 16, 1, 0, 0, time.UTC)},
		{"*/30 * * * *", time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC)},
		{"@daily", time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			sched, err := ParseSchedule(tc.expr)
			if err != nil {
				t.Fatalf("ParseSchedule(%q): %v", tc.expr, err)
			}
			if got := sched.Next(base); !got.Equal(tc.want) {
				t.Fatalf("Next = %s, want %s", got, tc.want)
			}
		})
	}
}
