package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snmp161/logstat/internal/config"
)

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"INFO":  slog.LevelInfo,
		" warn": slog.LevelWarn,
		"error": slog.LevelError,
	}
	for name, want := range tests {
		got, err := ParseLevel(name)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", name, got, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil {
		t.Error("ParseLevel(trace) must fail")
	}
}

func TestFormatIsHumanReadable(t *testing.T) {
	var buf bytes.Buffer
	lg := NewWriter(&buf, slog.LevelDebug)
	lg.Info("flush done", "actions_written", 4, "errors", 0)

	line := strings.TrimRight(buf.String(), "\n")
	fields := strings.SplitN(line, " ", 3)
	if len(fields) != 3 {
		t.Fatalf("unexpected line %q", line)
	}
	if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
		t.Errorf("first field must be an RFC3339 timestamp, got %q: %v", fields[0], err)
	}
	if fields[1] != "INFO" {
		t.Errorf("second field = %q, want INFO", fields[1])
	}
	if !strings.HasPrefix(strings.TrimLeft(fields[2], " "), "flush done ") {
		t.Errorf("message missing: %q", fields[2])
	}
	if !strings.Contains(line, "actions_written=4") || !strings.Contains(line, "errors=0") {
		t.Errorf("attributes missing: %q", line)
	}
}

func TestValuesWithSpacesAreQuoted(t *testing.T) {
	var buf bytes.Buffer
	lg := NewWriter(&buf, slog.LevelDebug)
	lg.Debug("line matched", "line", `GET /a?x=get-sms HTTP/1.1`)
	if !strings.Contains(buf.String(), `line="GET /a?x=get-sms HTTP/1.1"`) {
		t.Fatalf("value not quoted: %q", buf.String())
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	lg := NewWriter(&buf, slog.LevelInfo)
	lg.Debug("per line noise")
	lg.Info("visible")
	lg.Warn("also visible")

	out := buf.String()
	if strings.Contains(out, "per line noise") {
		t.Error("debug records must be dropped at info level")
	}
	if !strings.Contains(out, "visible") || !strings.Contains(out, "also visible") {
		t.Errorf("info/warn records missing: %q", out)
	}
}

func TestWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	lg := NewWriter(&buf, slog.LevelDebug)
	lg.Logger.With("component", "tail").WithGroup("redis").Info("connected", "db", 3)
	out := buf.String()
	if !strings.Contains(out, "component=tail") || !strings.Contains(out, "redis.db=3") {
		t.Fatalf("attributes = %q", out)
	}
}

func TestFileOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logstat.log")

	lg, err := New(config.Logging{Level: "info", Output: config.OutputFile, File: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Debug("not written")
	lg.Info("written to the file")
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "written to the file") {
		t.Errorf("log file = %q", data)
	}
	if strings.Contains(string(data), "not written") {
		t.Error("debug record leaked into an info level log")
	}
}

func TestReopenAfterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logstat.log")

	lg, err := New(config.Logging{Level: "info", Output: config.OutputFile, File: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = lg.Close() }()

	lg.Info("before rotation")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := lg.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	lg.Info("after rotation")

	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rotated), "before rotation") || strings.Contains(string(rotated), "after rotation") {
		t.Errorf("rotated file = %q", rotated)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the log file must be recreated by Reopen: %v", err)
	}
	if !strings.Contains(string(current), "after rotation") {
		t.Errorf("current file = %q", current)
	}
}

func TestReopenIsNoopForStderr(t *testing.T) {
	lg, err := New(config.Logging{Level: "info", Output: config.OutputJournald})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := lg.Reopen(); err != nil {
		t.Errorf("Reopen on stderr = %v, want nil", err)
	}
	if err := lg.Close(); err != nil {
		t.Errorf("Close on stderr = %v, want nil", err)
	}
}

func TestNewRejectsBadLevelAndUnwritableFile(t *testing.T) {
	if _, err := New(config.Logging{Level: "trace", Output: config.OutputJournald}); err == nil {
		t.Error("New must reject an unknown level")
	}
	if _, err := New(config.Logging{
		Level:  "info",
		Output: config.OutputFile,
		File:   filepath.Join(t.TempDir(), "missing-dir", "x.log"),
	}); err == nil {
		t.Error("New must fail when the log file cannot be opened")
	}
}
