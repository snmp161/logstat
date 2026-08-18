package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/snmp161/logstat/internal/store"
)

// TestMain doubles as the entry point of the daemon when the test binary is
// re-executed with LOGSTAT_TEST_RUN_MAIN=1. That lets the signal handling, the
// lock file and the exit codes be tested against a real process.
func TestMain(m *testing.M) {
	if os.Getenv("LOGSTAT_TEST_RUN_MAIN") == "1" {
		os.Exit(run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func writeConfig(t *testing.T, dir string, mr *miniredis.Miniredis, extra string) string {
	t.Helper()
	host, port, err := net.SplitHostPort(mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "logstat.yaml")
	cfg := fmt.Sprintf(`log_path: %s
actions: [get-number, get-sms]
flush_interval: 3600
poll: true
lock_file: %s
redis:
  host: %s
  port: %s
  db: 0
logging:
  level: debug
  output: file
  file: %s
reset:
  enabled: false
%s`,
		filepath.Join(dir, "app.log"),
		filepath.Join(dir, "logstat.lock"),
		host, port,
		filepath.Join(dir, "logstat.log"),
		extra)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mustSet seeds a key directly in Redis, bypassing logstat.
func mustSet(t *testing.T, mr *miniredis.Miniredis, key, value string) {
	t.Helper()
	if err := mr.Set(key, value); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- argument handling -----------------------------------------------------

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no arguments", nil, 2},
		{"unknown command", []string{"nonsense"}, 2},
		{"version", []string{"version"}, 0},
		{"help", []string{"--help"}, 0},
		{"short help", []string{"-h"}, 0},
		{"help command", []string{"help"}, 0},
		{"run without config", []string{"run"}, 2},
		{"clear without config", []string{"clear", "--all"}, 2},
		{"run with a missing config", []string{"run", "--config", "/nonexistent/logstat.yaml"}, 1},
		{"run with an unknown flag", []string{"run", "--nope"}, 2},
		{"subcommand help", []string{"run", "-h"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Fatalf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestRunRejectsAnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logstat.yaml")
	if err := os.WriteFile(path, []byte("flush_interval: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"run", "--config", path}); got != 1 {
		t.Fatalf("exit code = %d, want 1", got)
	}
}

// --- clear ----------------------------------------------------------------

func TestClear(t *testing.T) {
	host := store.ShortHostname()

	t.Run("all", func(t *testing.T) {
		dir := t.TempDir()
		mr := miniredis.RunT(t)
		cfgPath := writeConfig(t, dir, mr, "")
		mustSet(t, mr, store.CounterKey(host, "get-number"), "42")
		mustSet(t, mr, store.CounterKey(host, "get-sms"), "7")

		if got := run([]string{"clear", "--config", cfgPath, "--all"}); got != 0 {
			t.Fatalf("exit code = %d, want 0", got)
		}
		for _, a := range []string{"get-number", "get-sms"} {
			if v, _ := mr.Get(store.CounterKey(host, a)); v != "0" {
				t.Errorf("%s counter = %q, want 0", a, v)
			}
			if v, _ := mr.Get(store.HeartbeatKey(host, a)); !strings.HasSuffix(v, "lines=0") {
				t.Errorf("%s value = %q, want lines=0", a, v)
			}
		}
	})

	t.Run("single action", func(t *testing.T) {
		dir := t.TempDir()
		mr := miniredis.RunT(t)
		cfgPath := writeConfig(t, dir, mr, "")
		mustSet(t, mr, store.CounterKey(host, "get-number"), "42")
		mustSet(t, mr, store.CounterKey(host, "get-sms"), "7")

		if got := run([]string{"clear", "-c", cfgPath, "--action", "get-sms"}); got != 0 {
			t.Fatalf("exit code = %d, want 0", got)
		}
		if v, _ := mr.Get(store.CounterKey(host, "get-sms")); v != "0" {
			t.Errorf("get-sms = %q, want 0", v)
		}
		if v, _ := mr.Get(store.CounterKey(host, "get-number")); v != "42" {
			t.Errorf("get-number = %q, want it untouched", v)
		}
	})

	t.Run("applies the configured ttl", func(t *testing.T) {
		dir := t.TempDir()
		mr := miniredis.RunT(t)
		// The config leaves redis.ttl at its default of one day.
		cfgPath := writeConfig(t, dir, mr, "")
		mustSet(t, mr, store.CounterKey(host, "get-number"), "42")

		if got := run([]string{"clear", "-c", cfgPath, "--all"}); got != 0 {
			t.Fatalf("exit code = %d, want 0", got)
		}
		for _, k := range []string{store.CounterKey(host, "get-number"), store.HeartbeatKey(host, "get-number")} {
			if got := mr.TTL(k); got != 24*time.Hour {
				t.Errorf("TTL of %s = %s, want 24h", k, got)
			}
		}
	})

	t.Run("without the heartbeat key", func(t *testing.T) {
		dir := t.TempDir()
		mr := miniredis.RunT(t)
		cfgPath := writeConfig(t, dir, mr, "heartbeat_key: false\n")
		mustSet(t, mr, store.CounterKey(host, "get-number"), "42")

		if got := run([]string{"clear", "-c", cfgPath, "--all"}); got != 0 {
			t.Fatalf("exit code = %d, want 0", got)
		}
		if v, _ := mr.Get(store.CounterKey(host, "get-number")); v != "0" {
			t.Errorf("counter = %q, want 0", v)
		}
		if mr.Exists(store.HeartbeatKey(host, "get-number")) {
			t.Error("clear must not create the heartbeat key when it is disabled")
		}
	})

	t.Run("rejects conflicting flags", func(t *testing.T) {
		dir := t.TempDir()
		mr := miniredis.RunT(t)
		cfgPath := writeConfig(t, dir, mr, "")
		if got := run([]string{"clear", "-c", cfgPath, "--all", "--action", "get-sms"}); got != 2 {
			t.Errorf("--all with --action must be a usage error, got %d", got)
		}
		if got := run([]string{"clear", "-c", cfgPath}); got != 2 {
			t.Errorf("neither --all nor --action must be a usage error, got %d", got)
		}
		if got := run([]string{"clear", "-c", cfgPath, "--action", "not-configured"}); got != 2 {
			t.Errorf("an unconfigured action must be a usage error, got %d", got)
		}
	})

	t.Run("reports a Redis failure", func(t *testing.T) {
		dir := t.TempDir()
		mr := miniredis.RunT(t)
		cfgPath := writeConfig(t, dir, mr, "")
		mr.Close()
		if got := run([]string{"clear", "-c", cfgPath, "--all"}); got != 1 {
			t.Errorf("exit code = %d, want 1", got)
		}
	})
}

// --- the daemon as a real process ------------------------------------------

func daemonCmd(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...) //nolint:gosec // the test binary re-executes itself
	cmd.Env = append(os.Environ(), "LOGSTAT_TEST_RUN_MAIN=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// TestDaemonGracefulShutdown runs the real binary, feeds it log lines and stops
// it with SIGTERM: the buffered increments must reach Redis in the final flush
// (flush_interval is an hour, so nothing else can have written them) and the
// process must exit with code 0.
func TestDaemonGracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: runs the daemon as a separate process")
	}
	dir := t.TempDir()
	mr := miniredis.RunT(t)
	cfgPath := writeConfig(t, dir, mr, "")
	logPath := filepath.Join(dir, "app.log")
	daemonLog := filepath.Join(dir, "logstat.log")
	host := store.ShortHostname()

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := daemonCmd(t, "run", "--config", cfgPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	waitFor(t, "the daemon to attach to the log file", func() bool {
		return strings.Contains(readFile(t, daemonLog), "tailing from its end")
	})
	waitFor(t, "the Redis keys to be initialised", func() bool {
		v, _ := mr.Get(store.CounterKey(host, "get-number"))
		return v == "0"
	})

	// Append until the daemon reports at least three matched lines. How many of
	// the first writes it catches depends on when it finished seeking, so the
	// expected total is derived from what it actually logged.
	waitFor(t, "the daemon to count some lines", func() bool {
		appendLine(t, logPath, "GET /api/get-number and get-sms")
		time.Sleep(100 * time.Millisecond)
		return strings.Count(readFile(t, daemonLog), "line matched") >= 3
	})
	matched := strings.Count(readFile(t, daemonLog), "line matched")

	// Nothing may have been written to Redis yet: the flush interval is an hour.
	if v, _ := mr.Get(store.CounterKey(host, "get-number")); v != "0" {
		t.Fatalf("counter = %q before the final flush, want 0", v)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	killed = true
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the daemon must exit with code 0 on SIGTERM, got %v", err)
	}

	want := strconv.Itoa(matched)
	for _, a := range []string{"get-number", "get-sms"} {
		if v, _ := mr.Get(store.CounterKey(host, a)); v != want {
			t.Errorf("%s counter = %q, want %s (the final flush must persist the buffer)", a, v, want)
		}
		if v, _ := mr.Get(store.HeartbeatKey(host, a)); !strings.HasSuffix(v, "lines="+want) {
			t.Errorf("%s value = %q, want lines=%s", a, v, want)
		}
	}
	out := readFile(t, daemonLog)
	if !strings.Contains(out, "shutting down, flushing buffer") || !strings.Contains(out, "stopped") {
		t.Errorf("daemon log = %q", out)
	}
}

// TestDaemonSecondInstanceIsRejected covers the flock guard: two processes with
// the same lock_file must not both run.
func TestDaemonSecondInstanceIsRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: runs the daemon as a separate process")
	}
	dir := t.TempDir()
	mr := miniredis.RunT(t)
	cfgPath := writeConfig(t, dir, mr, "")
	daemonLog := filepath.Join(dir, "logstat.log")

	first := daemonCmd(t, "run", "--config", cfgPath)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = first.Process.Signal(syscall.SIGTERM)
		_ = first.Wait()
	}()

	waitFor(t, "the first instance to take the lock", func() bool {
		return strings.Contains(readFile(t, daemonLog), "waiting for it") ||
			strings.Contains(readFile(t, daemonLog), "tailing from its end")
	})

	second := daemonCmd(t, "run", "--config", cfgPath)
	err := second.Run()
	if err == nil {
		t.Fatal("the second instance must fail")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("second instance exited with %v, want exit code 1", err)
	}
	if !strings.Contains(readFile(t, daemonLog), "cannot acquire lock") {
		t.Errorf("the lock failure must be logged: %q", readFile(t, daemonLog))
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
