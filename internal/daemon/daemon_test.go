package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"

	"github.com/snmp161/logstat/internal/config"
	"github.com/snmp161/logstat/internal/logging"
	"github.com/snmp161/logstat/internal/store"
)

const testHost = "testhost"

// syncBuffer is a bytes.Buffer safe for the concurrent writes of the daemon.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	d   *Daemon
	mr  *miniredis.Miniredis
	log *syncBuffer
	cfg *config.Config
}

func newHarness(t *testing.T, tweak func(*config.Config), opts ...Option) *harness {
	t.Helper()
	mr := miniredis.RunT(t)
	return newHarnessAt(t, mr, tweak, opts...)
}

func newHarnessAt(t *testing.T, mr *miniredis.Miniredis, tweak func(*config.Config), opts ...Option) *harness {
	t.Helper()

	cfg := config.Default()
	cfg.Actions = []string{"get-number", "get-sms", "getNumber", "getStatus"}
	cfg.FlushInterval = 1
	cfg.LogPath = filepath.Join(t.TempDir(), "app.log")
	cfg.Reset.Enabled = false
	cfg.Redis.TTL = 0
	if tweak != nil {
		tweak(&cfg)
	}
	if _, err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}

	buf := &syncBuffer{}
	lg := logging.NewWriter(buf, slog.LevelDebug)

	store.SetLibraryLogger(lg.Logger)

	rdb := redis.NewClient(&redis.Options{
		Addr:         mr.Addr(),
		DialTimeout:  time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	return &harness{
		d:   New(&cfg, lg.Logger, store.NewWithClient(rdb, testHost, cfg.HeartbeatKey, cfg.Redis.TTLDuration()), opts...),
		mr:  mr,
		log: buf,
		cfg: &cfg,
	}
}

func (h *harness) counter(t *testing.T, action string) string {
	t.Helper()
	v, err := h.mr.Get(store.CounterKey(testHost, action))
	if err != nil {
		return "<missing>"
	}
	return v
}

func (h *harness) heartbeat(t *testing.T, action string) string {
	t.Helper()
	v, err := h.mr.Get(store.HeartbeatKey(testHost, action))
	if err != nil {
		return "<missing>"
	}
	return v
}

// waitFor polls cond until it holds or the deadline expires.
func (h *harness) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\ndaemon log:\n%s", what, h.log.String())
}

// mustSet seeds a key directly in Redis, bypassing the daemon.
func mustSet(t *testing.T, mr *miniredis.Miniredis, key, value string) {
	t.Helper()
	if err := mr.Set(key, value); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

// --- unit level: flush and reset ------------------------------------------

func TestFlushWritesCounterAndHeartbeat(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	h.d.Counter().ProcessLine("a get-number b")
	h.d.Counter().ProcessLine("a get-number and get-sms b")
	h.d.Flush(ctx)

	if got := h.counter(t, "get-number"); got != "2" {
		t.Errorf("get-number counter = %s, want 2", got)
	}
	if got := h.counter(t, "get-sms"); got != "1" {
		t.Errorf("get-sms counter = %s, want 1", got)
	}
	if got := h.heartbeat(t, "get-number"); !strings.HasSuffix(got, "lines=2") ||
		!strings.HasPrefix(got, "server=testhost time=") ||
		!strings.Contains(got, "type=get-number") {
		t.Errorf("get-number heartbeat = %q", got)
	}
	if !h.mr.Exists("logstat:heartbeat:testhost:get-number") {
		t.Errorf("heartbeat key has the wrong name, keys = %v", h.mr.Keys())
	}
	// Untouched actions are initialised to zero, not left missing.
	if got := h.counter(t, "getStatus"); got != "0" {
		t.Errorf("getStatus counter = %s, want 0", got)
	}

	// The buffer is empty afterwards and the totals keep growing.
	h.d.Counter().ProcessLine("get-number")
	h.d.Flush(ctx)
	if got := h.counter(t, "get-number"); got != "3" {
		t.Errorf("get-number counter = %s, want 3", got)
	}
}

// The daemon has to hand case_sensitive to the counter it builds, and the case
// of the log line must not reach the key names: whatever the line looked like,
// the total belongs to the word as spelled in actions.
func TestFlushInCaseInsensitiveMode(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.CaseSensitive = false })

	h.d.Counter().ProcessLine("a GET-NUMBER b")
	h.d.Counter().ProcessLine("a Get-Number and getstatus b")
	h.d.Flush(context.Background())

	if got := h.counter(t, "get-number"); got != "2" {
		t.Errorf("get-number counter = %s, want 2", got)
	}
	if got := h.counter(t, "getStatus"); got != "1" {
		t.Errorf("getStatus counter = %s, want 1", got)
	}
	if got := h.heartbeat(t, "getStatus"); !strings.Contains(got, "type=getStatus") {
		t.Errorf("getStatus heartbeat = %q, want the configured spelling", got)
	}
	for _, key := range h.mr.Keys() {
		if strings.Contains(key, "GET-NUMBER") || strings.Contains(key, "getstatus") {
			t.Errorf("key %q takes its spelling from the log line, keys = %v", key, h.mr.Keys())
		}
	}
}

// The default mode is unchanged by the new option: a differently cased line is
// simply not a match.
func TestFlushInCaseSensitiveModeIgnoresOtherSpellings(t *testing.T) {
	h := newHarness(t, nil)

	h.d.Counter().ProcessLine("a GET-NUMBER b")
	h.d.Flush(context.Background())

	if got := h.counter(t, "get-number"); got != "0" {
		t.Errorf("get-number counter = %s, want 0", got)
	}
}

// With heartbeat_key off the daemon maintains the integer counters only.
func TestFlushWithoutHeartbeatKey(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.HeartbeatKey = false })
	ctx := context.Background()

	h.d.Counter().ProcessLine("a get-number b")
	h.d.Counter().ProcessLine("get-sms")
	h.d.Flush(ctx)

	if got := h.counter(t, "get-number"); got != "1" {
		t.Errorf("get-number counter = %s, want 1", got)
	}
	if got := h.heartbeat(t, "get-number"); got != "<missing>" {
		t.Errorf("heartbeat key = %q, want it absent", got)
	}

	// A reset keeps the counters at zero and still writes no heartbeat.
	h.d.Reset(ctx)
	if got := h.counter(t, "get-number"); got != "0" {
		t.Errorf("get-number counter after reset = %s, want 0", got)
	}
	for _, a := range h.cfg.Actions {
		if got := h.heartbeat(t, a); got != "<missing>" {
			t.Errorf("%s heartbeat = %q, want it absent", a, got)
		}
	}

	// Only the four counter keys exist.
	if got := h.mr.Keys(); len(got) != len(h.cfg.Actions) {
		t.Errorf("keys = %v, want only the %d counters", got, len(h.cfg.Actions))
	}
}

func TestFlushDoesNotOverwriteAnExistingCounter(t *testing.T) {
	h := newHarness(t, nil)
	mustSet(t, h.mr, store.CounterKey(testHost, "get-sms"), "100")

	h.d.Counter().ProcessLine("get-sms")
	h.d.Flush(context.Background())

	if got := h.counter(t, "get-sms"); got != "101" {
		t.Fatalf("counter = %s, want 101 (a restart must not lose the accumulated value)", got)
	}
}

func TestFlushKeepsBufferWhenRedisIsDown(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	h := newHarnessAt(t, mr, nil)
	ctx := context.Background()

	h.d.Counter().ProcessLine("get-number")
	h.d.Flush(ctx)
	if got := h.counter(t, "get-number"); got != "1" {
		t.Fatalf("counter = %s, want 1", got)
	}

	mr.Close()
	h.d.Counter().ProcessLine("get-number")
	h.d.Counter().ProcessLine("get-sms")
	h.d.Flush(ctx)

	if got := h.d.Counter().Pending(); got != 2 {
		t.Fatalf("pending = %d, want 2 (increments must survive an outage)", got)
	}
	if !strings.Contains(h.log.String(), "redis unavailable") {
		t.Errorf("the outage must be logged: %q", h.log.String())
	}

	// Redis comes back at the same address; go-redis reconnects on its own.
	revived := miniredis.NewMiniRedis()
	if err := revived.StartAddr(addr); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}
	defer revived.Close()
	mustSet(t, revived, store.CounterKey(testHost, "get-number"), "1")

	h.d.Flush(ctx)
	if got := h.d.Counter().Pending(); got != 0 {
		t.Fatalf("pending after recovery = %d, want 0", got)
	}
	if got, _ := revived.Get(store.CounterKey(testHost, "get-number")); got != "2" {
		t.Errorf("get-number = %s, want 2", got)
	}
	if got, _ := revived.Get(store.HeartbeatKey(testHost, "get-sms")); !strings.HasSuffix(got, "lines=1") {
		t.Errorf("get-sms value = %q, want lines=1", got)
	}
	if !strings.Contains(h.log.String(), "redis is back") {
		t.Errorf("the recovery must be logged: %q", h.log.String())
	}
}

// Keys of a running daemon must never expire: the flush re-applies the TTL, and
// it has to do so even when there was nothing to write, otherwise the counter of
// a quiet action would silently lose its total.
func TestFlushAppliesAndRefreshesTTL(t *testing.T) {
	const ttl = time.Hour
	h := newHarness(t, func(c *config.Config) { c.Redis.TTL = int(ttl.Seconds()) })
	ctx := context.Background()

	h.d.Counter().ProcessLine("get-number")
	h.d.Flush(ctx)

	counterKey := store.CounterKey(testHost, "get-number")
	beatKey := store.HeartbeatKey(testHost, "get-number")
	for _, k := range []string{counterKey, beatKey} {
		if got := h.mr.TTL(k); got != ttl {
			t.Fatalf("TTL of %s = %s, want %s", k, got, ttl)
		}
	}

	// Two thirds of the way to the expiry, with no new lines at all.
	h.mr.FastForward(40 * time.Minute)
	if got := h.mr.TTL(counterKey); got != 20*time.Minute {
		t.Fatalf("TTL = %s, want 20m before the idle flush", got)
	}
	h.d.Flush(ctx)
	for _, k := range []string{counterKey, beatKey} {
		if got := h.mr.TTL(k); got != ttl {
			t.Errorf("TTL of %s after an idle flush = %s, want it refreshed to %s", k, got, ttl)
		}
	}

	// The idle flush refreshed rather than counted.
	if got, _ := h.mr.Get(counterKey); got != "1" {
		t.Errorf("counter = %q, want 1", got)
	}
}

// Switching the expiry off has to take effect on keys that already carry one.
func TestFlushWithTTLZeroClearsAnExistingExpiry(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Redis.TTL = 0 })
	counterKey := store.CounterKey(testHost, "get-number")

	mustSet(t, h.mr, counterKey, "5")
	h.mr.SetTTL(counterKey, time.Hour)

	h.d.Flush(context.Background())

	if got := h.mr.TTL(counterKey); got != 0 {
		t.Errorf("TTL = %s, want it cleared", got)
	}
	h.mr.FastForward(2 * time.Hour)
	if got, _ := h.mr.Get(counterKey); got != "5" {
		t.Errorf("counter = %q, want 5: the key must not expire any more", got)
	}
}

func TestResetZeroesEveryCounter(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	h.d.Counter().ProcessLine("get-number get-sms getNumber getStatus")
	h.d.Flush(ctx)
	if got := h.counter(t, "getStatus"); got != "1" {
		t.Fatalf("counter = %s, want 1", got)
	}

	h.d.Reset(ctx)

	for _, a := range h.cfg.Actions {
		if got := h.counter(t, a); got != "0" {
			t.Errorf("%s counter after reset = %s, want 0", a, got)
		}
		if got := h.heartbeat(t, a); !strings.HasSuffix(got, "lines=0") {
			t.Errorf("%s heartbeat after reset = %q, want lines=0", a, got)
		}
	}
}

// Lines buffered when the schedule fires belong to the interval that ends, so
// they must reach Redis before the zeroing, not after it.
func TestResetFlushesTheBufferFirst(t *testing.T) {
	h := newHarness(t, nil)

	h.d.Counter().ProcessLine("get-number")
	h.d.Reset(context.Background())

	if got := h.counter(t, "get-number"); got != "0" {
		t.Errorf("counter = %s, want 0", got)
	}
	if got := h.d.Counter().Pending(); got != 0 {
		t.Errorf("pending = %d, want 0: the buffer must have been flushed", got)
	}
	// The intermediate INCRBY happened: the log records a flush before the reset.
	out := h.log.String()
	if i, j := strings.Index(out, "flush done"), strings.Index(out, "counters reset"); i < 0 || j < 0 || i > j {
		t.Errorf("expected a flush before the reset, log = %q", out)
	}
}

func TestResetIsRetriedAfterAnOutage(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	h := newHarnessAt(t, mr, nil)
	ctx := context.Background()

	h.d.Counter().ProcessLine("get-number")
	h.d.Flush(ctx)
	mr.Close()

	h.d.Reset(ctx) // fails, must be remembered
	if len(h.d.pendingResets) != len(h.cfg.Actions) {
		t.Fatalf("pendingResets = %v, want every action", h.d.pendingResets)
	}

	revived := miniredis.NewMiniRedis()
	if err := revived.StartAddr(addr); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}
	defer revived.Close()
	mustSet(t, revived, store.CounterKey(testHost, "get-number"), "1")

	h.d.Flush(ctx)
	if len(h.d.pendingResets) != 0 {
		t.Fatalf("pendingResets = %v, want empty", h.d.pendingResets)
	}
	if got, _ := revived.Get(store.CounterKey(testHost, "get-number")); got != "0" {
		t.Errorf("counter = %s, want 0", got)
	}
}

// --- integration level: the full Run loop ----------------------------------

// runDaemon starts d.Run in the background and returns a stop function that
// cancels it and waits for the final flush.
func runDaemon(t *testing.T, d *Daemon) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("Run did not return after cancellation")
		}
	}
	t.Cleanup(stop)
	return stop
}

// waitTailStarted waits until the daemon has chosen its start position in the
// log file, i.e. the tailer has been created.
func (h *harness) waitTailStarted(t *testing.T) {
	t.Helper()
	h.waitFor(t, "the tailer to pick a start position", func() bool {
		out := h.log.String()
		return strings.Contains(out, "tailing from its end") ||
			strings.Contains(out, "does not exist yet")
	})
}

// waitTailAttached blocks until the tailer really delivers lines from the
// current log file. It appends neutral lines (matching no action) until the
// counter sees them, which is the only observable proof that the tailer has
// opened the file and positioned itself — a plain sleep would either be flaky
// or needlessly slow.
func waitTailAttached(t *testing.T, h *harness) {
	t.Helper()
	before := h.d.Counter().Lines()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		appendLines(t, h.cfg.LogPath, "warmup line with no code word")
		for i := 0; i < 20; i++ {
			if h.d.Counter().Lines() > before {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	t.Fatalf("the tailer never attached to the log file\ndaemon log:\n%s", h.log.String())
}

func TestRunStartsWithoutTheLogFileAndPicksItUpLater(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	h := newHarness(t, func(c *config.Config) { c.Poll = true })

	if _, err := os.Stat(h.cfg.LogPath); !os.IsNotExist(err) {
		t.Fatalf("the log file must not exist yet: %v", err)
	}
	runDaemon(t, h.d)

	// Wait until the daemon has decided to wait for the file: only then is it
	// guaranteed to read the file from its beginning instead of seeking to its end.
	h.waitFor(t, "the daemon to start waiting for the log file", func() bool {
		return strings.Contains(h.log.String(), "log file does not exist yet")
	})
	// The keys are bootstrapped even though there is nothing to tail yet.
	h.waitFor(t, "key initialisation", func() bool { return h.counter(t, "get-number") == "0" })

	appendLines(t, h.cfg.LogPath, "first get-number", "second get-sms")
	h.waitFor(t, "the appearing file to be tailed", func() bool { return h.counter(t, "get-number") == "1" })
	h.waitFor(t, "get-sms", func() bool { return h.counter(t, "get-sms") == "1" })
}

func TestRunSkipsPreexistingLinesAndCountsNewOnes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	h := newHarness(t, nil)
	appendLines(t, h.cfg.LogPath, "old get-number", "old get-number", "old get-sms")

	runDaemon(t, h.d)
	h.waitTailStarted(t)
	waitTailAttached(t, h)

	appendLines(t, h.cfg.LogPath, "new get-number")
	h.waitFor(t, "the new line", func() bool { return h.counter(t, "get-number") == "1" })

	if got := h.counter(t, "get-sms"); got != "0" {
		t.Errorf("get-sms = %s, want 0: lines written before the start must be ignored", got)
	}
}

// The whole path from the tailed line to the Redis key in the case-insensitive
// mode: a line whose spelling differs from the config still lands on the key of
// the configured word.
func TestRunCountsAnyCaseWhenCaseSensitiveIsOff(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	h := newHarness(t, func(c *config.Config) { c.CaseSensitive = false })

	runDaemon(t, h.d)
	h.waitTailStarted(t)
	waitTailAttached(t, h)

	appendLines(t, h.cfg.LogPath, "new GET-NUMBER", "new Get-Number")
	h.waitFor(t, "both spellings on one key", func() bool { return h.counter(t, "get-number") == "2" })
}

func TestRunSurvivesRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	for _, mode := range []string{"create", "copytruncate"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, func(c *config.Config) { c.Poll = true })
			path := h.cfg.LogPath
			runDaemon(t, h.d)
			h.waitTailStarted(t)
			waitTailAttached(t, h)

			appendLines(t, path, "before get-number")
			h.waitFor(t, "the pre-rotation line", func() bool { return h.counter(t, "get-number") == "1" })

			switch mode {
			case "create":
				// logrotate create: the old file is renamed away and the source
				// recreates it, so the inode changes.
				if err := os.Rename(path, path+".1"); err != nil {
					t.Fatal(err)
				}
			case "copytruncate":
				// logrotate copytruncate: the content is copied away and the
				// file is truncated in place, so the size jumps backwards.
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".1", data, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			}

			// Let the watcher observe the rotation before the source writes again.
			time.Sleep(750 * time.Millisecond)
			waitTailAttached(t, h)

			appendLines(t, path, "after get-number")
			h.waitFor(t, "the post-rotation line", func() bool { return h.counter(t, "get-number") == "2" })

			// Give the tailer a chance to double count and make sure it does not.
			time.Sleep(1500 * time.Millisecond)
			if got := h.counter(t, "get-number"); got != "2" {
				t.Fatalf("get-number = %s, want 2: rotation must not replay lines", got)
			}
		})
	}
}

func TestRunSchedulerFiresTheReset(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	// A six-field expression firing every second keeps the test at a couple of
	// seconds instead of waiting for a real cron minute.
	h := newHarness(t,
		func(c *config.Config) {
			c.Poll = true
			c.Reset.Enabled = true
		},
		WithCronOptions(cron.WithSeconds()),
	)
	// Set after validation: the validator only accepts standard 5-field cron.
	h.cfg.Reset.Schedule = "* * * * * *"

	// Seed a non-zero counter, as if the previous interval had been counted.
	// Watching the seeded value drop to zero is race free, while watching a
	// counter that the scheduler zeroes every second would not be.
	mustSet(t, h.mr, store.CounterKey(testHost, "get-number"), "42")

	runDaemon(t, h.d)

	h.waitFor(t, "the scheduled reset", func() bool {
		return h.counter(t, "get-number") == "0" &&
			strings.HasSuffix(h.heartbeat(t, "get-number"), "lines=0")
	})
	h.waitFor(t, "the reset to be logged", func() bool {
		out := h.log.String()
		return strings.Contains(out, "scheduled reset fired") && strings.Contains(out, "counters reset")
	})
}

func TestRunWithoutSchedulerNeverResets(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	h := newHarness(t, func(c *config.Config) { c.Poll = true })
	runDaemon(t, h.d)
	h.waitTailStarted(t)
	waitTailAttached(t, h)

	appendLines(t, h.cfg.LogPath, "get-number")
	h.waitFor(t, "the line to be counted", func() bool { return h.counter(t, "get-number") == "1" })

	time.Sleep(2 * time.Second)
	if got := h.counter(t, "get-number"); got != "1" {
		t.Fatalf("counter = %s, want 1: nothing may reset it when reset.enabled is false", got)
	}
	if !strings.Contains(h.log.String(), "self-reset disabled") {
		t.Errorf("log = %q", h.log.String())
	}
	if strings.Contains(h.log.String(), "counters reset") {
		t.Errorf("nothing may reset the counters: %q", h.log.String())
	}
}

func TestGracefulShutdownFlushesTheBuffer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	// A long flush interval guarantees the lines are still buffered at shutdown.
	h := newHarness(t, func(c *config.Config) {
		c.Poll = true
		c.FlushInterval = 3600
	})
	stop := runDaemon(t, h.d)
	h.waitTailStarted(t)
	waitTailAttached(t, h)

	appendLines(t, h.cfg.LogPath, "get-number", "get-number and get-sms")
	h.waitFor(t, "the lines to be buffered", func() bool { return h.d.Counter().Pending() == 3 })

	stop()

	if got := h.counter(t, "get-number"); got != "2" {
		t.Errorf("get-number = %s, want 2 after the final flush", got)
	}
	if got := h.counter(t, "get-sms"); got != "1" {
		t.Errorf("get-sms = %s, want 1 after the final flush", got)
	}
	out := h.log.String()
	if !strings.Contains(out, "shutting down") || !strings.Contains(out, "stopped") {
		t.Errorf("shutdown must be logged: %q", out)
	}
}

func TestRunRejectsABadSchedule(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Poll = true
		c.Reset.Enabled = true
	})
	// Bypass the config validator to reach the scheduler's own error path.
	h.cfg.Reset.Schedule = "not a cron expression"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.d.Run(ctx); err == nil {
		t.Fatal("Run must fail on an unparsable schedule")
	}
}

// --- integration level: the metrics exporter -------------------------------

// waitMetricsAddr blocks until Run has bound the exporter and returns its
// address. The port is picked by the kernel, so the test cannot know it upfront.
func (h *harness) waitMetricsAddr(t *testing.T) string {
	t.Helper()
	var addr string
	h.waitFor(t, "the metrics exporter to bind", func() bool {
		addr = h.d.MetricsAddr()
		return addr != ""
	})
	return addr
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestRunServesMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	h := newHarness(t, func(c *config.Config) {
		c.Poll = true
		c.Metrics.Enabled = true
	})
	// Port 0 means "any free port", which is what a test needs and what the
	// config validator rightly refuses from an operator, so it is set past the
	// validation rather than through the config.
	h.cfg.Metrics.Listen = "127.0.0.1:0"

	stop := runDaemon(t, h.d)
	addr := h.waitMetricsAddr(t)
	h.waitTailStarted(t)
	waitTailAttached(t, h)

	appendLines(t, h.cfg.LogPath, "one get-number", "another get-number")
	h.waitFor(t, "the lines to reach Redis", func() bool { return h.counter(t, "get-number") == "2" })

	code, body := httpGet(t, "http://"+addr+"/metrics")
	if code != 200 {
		t.Fatalf("GET /metrics = %d, want 200", code)
	}
	for _, want := range []string{
		`logstat_matched_lines_total{action="get-number"} 2`,
		`logstat_redis_counter{action="get-number"} 2`,
		"logstat_redis_up 1",
		"logstat_config_info{",
		"logstat_config_flush_interval_seconds 1",
		"logstat_uptime_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the response does not contain %q\nbody:\n%s", want, body)
		}
	}
	// The root is the status page for a browser, built from the same numbers.
	code, page := httpGet(t, "http://"+addr+"/")
	if code != 200 {
		t.Fatalf("GET / = %d, want 200", code)
	}
	for _, want := range []string{"logstat", "get-number", h.cfg.LogPath, "/metrics"} {
		if !strings.Contains(page, want) {
			t.Errorf("the status page does not contain %q\nbody:\n%s", want, page)
		}
	}
	if code, _ := httpGet(t, "http://"+addr+"/nope"); code != 404 {
		t.Errorf("GET /nope = %d, want 404", code)
	}

	// The exporter goes down with the daemon and gives the port back.
	stop()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the metrics port is still taken after the shutdown: %v", err)
	}
	_ = ln.Close()
}

func TestRunWithoutMetricsListensNowhere(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	h := newHarness(t, func(c *config.Config) { c.Poll = true })
	runDaemon(t, h.d)
	h.waitTailStarted(t)
	waitTailAttached(t, h)

	if got := h.d.MetricsAddr(); got != "" {
		t.Fatalf("MetricsAddr = %q, want empty with metrics.enabled false", got)
	}
}

// A taken port is fatal: running on without metrics that were explicitly
// switched on would leave the monitoring side looking at silence.
func TestRunFailsWhenTheMetricsPortIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	h := newHarness(t, func(c *config.Config) {
		c.Poll = true
		c.Metrics.Enabled = true
		c.Metrics.Listen = ln.Addr().String()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = h.d.Run(ctx)
	if err == nil {
		t.Fatal("Run must fail when the metrics port is already in use")
	}
	if !strings.Contains(err.Error(), ln.Addr().String()) {
		t.Errorf("error = %v, want it to name the address", err)
	}
	// The socket is opened before the log file is touched, so the failure is
	// immediate instead of arriving after the first flush.
	if out := h.log.String(); strings.Contains(out, "log file") {
		t.Errorf("the daemon started tailing before failing on the metrics port:\n%s", out)
	}
}

// An exporter that dies while the daemon runs is as fatal as a port that could
// not be bound in the first place: counting on without the metrics somebody
// switched on leaves the monitoring side reading silence as health.
func TestRunStopsWhenTheExporterDies(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	h := newHarness(t, func(c *config.Config) {
		c.Poll = true
		c.Metrics.Enabled = true
	})
	h.cfg.Metrics.Listen = "127.0.0.1:0" // see TestRunServesMetrics

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- h.d.Run(ctx) }()

	h.waitMetricsAddr(t)
	waitTailAttached(t, h)
	appendLines(t, h.cfg.LogPath, "one get-number")
	// The line has to be counted before the exporter dies, or the assertion
	// below would pass on an empty buffer and prove nothing.
	h.waitFor(t, "the line to be counted", func() bool { return h.d.Counter().Matched()["get-number"] == 1 })

	// What a fatal accept error would look like from the daemon's side.
	h.d.metricsErr <- errors.New("listener died")

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Run must return the error the exporter died with")
		}
		if !strings.Contains(err.Error(), "listener died") {
			t.Errorf("Run returned %v, want the exporter error", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("Run did not return\ndaemon log:\n%s", h.log.String())
	}

	// The buffer still reaches Redis: a fatal exporter is not a reason to drop
	// what was already counted.
	h.waitFor(t, "the final flush", func() bool { return h.counter(t, "get-number") == "1" })
}

// The normal path must stay quiet: a graceful shutdown of the exporter is not
// an error and must not be mistaken for one.
func TestRunIgnoresACleanExporterStop(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: tails a real file over several seconds")
	}
	h := newHarness(t, func(c *config.Config) {
		c.Poll = true
		c.Metrics.Enabled = true
	})
	h.cfg.Metrics.Listen = "127.0.0.1:0"

	stop := runDaemon(t, h.d)
	h.waitMetricsAddr(t)
	waitTailAttached(t, h)

	stop() // runDaemon fails the test if Run returns an error here

	if len(h.d.metricsErr) != 0 {
		t.Error("a clean shutdown must not queue an exporter error")
	}
}

// After an outage the daemon says so once, and it has to say so even if nothing
// was counted meanwhile: the TTL refresh of an idle flush is a Redis round trip
// like any other, and it is what notices the recovery.
func TestIdleFlushNoticesRedisComingBack(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Redis.TTL = 3600 })
	ctx := context.Background()

	h.d.Flush(ctx) // bootstraps the keys while Redis is healthy

	h.mr.SetError("LOADING Redis is loading the dataset in memory")
	h.d.Flush(ctx) // nothing buffered: only the TTL refresh talks to Redis
	if !strings.Contains(h.log.String(), "redis unavailable") {
		t.Fatalf("the outage must be logged once:\n%s", h.log.String())
	}

	h.mr.SetError("")
	h.d.Flush(ctx)
	if !strings.Contains(h.log.String(), "redis is back") {
		t.Fatalf("an idle flush must notice the recovery:\n%s", h.log.String())
	}
}

func TestPerLineLoggingOnlyAtDebug(t *testing.T) {
	mr := miniredis.RunT(t)
	buf := &syncBuffer{}
	lg := logging.NewWriter(buf, slog.LevelInfo)

	cfg := config.Default()
	cfg.LogPath = filepath.Join(t.TempDir(), "app.log")
	cfg.Reset.Enabled = false
	cfg.Redis.TTL = 0

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	d := New(&cfg, lg.Logger, store.NewWithClient(rdb, testHost, cfg.HeartbeatKey, cfg.Redis.TTLDuration()))

	d.Counter().ProcessLine("get-number")
	d.Flush(context.Background())

	if strings.Contains(buf.String(), "line matched") || strings.Contains(buf.String(), "flush done") {
		t.Fatalf("per-line and per-flush debug records must not appear at info level: %q", buf.String())
	}
}

func TestMiniredisAddrIsParsable(t *testing.T) {
	// Guards the helper used by the store tests: miniredis always binds 127.0.0.1.
	mr := miniredis.RunT(t)
	h, p, err := net.SplitHostPort(mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.Atoi(p); err != nil || h == "" {
		t.Fatalf("addr = %q", mr.Addr())
	}
}
