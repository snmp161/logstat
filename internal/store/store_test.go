package store

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/snmp161/logstat/internal/config"
)

const host = "web01"

var ts = time.Date(2026, 8, 18, 15, 4, 5, 0, time.FixedZone("MSK", 3*3600))

func newStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	return newTestStore(t, true, 0)
}

func newStoreWithHeartbeat(t *testing.T, heartbeat bool) (*Store, *miniredis.Miniredis) {
	t.Helper()
	return newTestStore(t, heartbeat, 0)
}

func newTestStore(t *testing.T, heartbeat bool, ttl time.Duration) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewWithClient(rdb, host, heartbeat, ttl), mr
}

func TestKeys(t *testing.T) {
	if got, want := CounterKey(host, "get-sms"), "logstat:counter:web01:get-sms"; got != want {
		t.Errorf("CounterKey = %q, want %q", got, want)
	}
	if got, want := HeartbeatKey(host, "get-sms"), "logstat:heartbeat:web01:get-sms"; got != want {
		t.Errorf("HeartbeatKey = %q, want %q", got, want)
	}
	if !strings.HasPrefix(CounterKey(host, "x"), KeyPrefix) || !strings.HasPrefix(HeartbeatKey(host, "x"), KeyPrefix) {
		t.Error("every key must start with the logstat: prefix")
	}
}

func TestFormatHeartbeat(t *testing.T) {
	got := FormatHeartbeat(host, ts, "getStatus", 42)
	want := "server=web01 time=2026-08-18T15:04:05+03:00 type=getStatus lines=42"
	if got != want {
		t.Fatalf("FormatHeartbeat =\n%q\nwant\n%q", got, want)
	}
	// The timestamp must be the same shape as `date -Iseconds`.
	if got := FormatHeartbeat(host, ts.UTC(), "x", 0); !strings.Contains(got, "time=2026-08-18T12:04:05Z") {
		t.Fatalf("UTC timestamp: %q", got)
	}
}

func TestShortHostname(t *testing.T) {
	h := ShortHostname()
	if h == "" {
		t.Fatal("ShortHostname must not be empty")
	}
	if strings.Contains(h, ".") {
		t.Fatalf("ShortHostname must be the short form, got %q", h)
	}
}

func TestInitCreatesMissingKeysOnly(t *testing.T) {
	st, mr := newStore(t)
	ctx := context.Background()
	actions := []string{"a", "b"}

	// "a" already carries a value from a previous run of the daemon.
	if err := mr.Set(CounterKey(host, "a"), "17"); err != nil {
		t.Fatal(err)
	}

	if err := st.Init(ctx, actions, ts); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if got, _ := mr.Get(CounterKey(host, "a")); got != "17" {
		t.Errorf("existing counter overwritten: %q", got)
	}
	if got, _ := mr.Get(CounterKey(host, "b")); got != "0" {
		t.Errorf("new counter = %q, want 0", got)
	}
	if got, _ := mr.Get(HeartbeatKey(host, "a")); !strings.HasSuffix(got, "lines=17") {
		t.Errorf("value of a = %q, want it to report the existing total", got)
	}
	if got, _ := mr.Get(HeartbeatKey(host, "b")); !strings.HasSuffix(got, "lines=0") {
		t.Errorf("value of b = %q", got)
	}

	// A second Init must be a no-op.
	if err := st.Init(ctx, actions, ts.Add(time.Hour)); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if got, _ := mr.Get(CounterKey(host, "a")); got != "17" {
		t.Errorf("second Init changed the counter: %q", got)
	}
}

// With heartbeat_key off only the integer counters exist: no monitoring value is
// created at init, written on a flush or touched on a reset.
func TestHeartbeatDisabled(t *testing.T) {
	st, mr := newStoreWithHeartbeat(t, false)
	ctx := context.Background()

	if st.HeartbeatEnabled() {
		t.Fatal("HeartbeatEnabled must be false")
	}

	if err := st.Init(ctx, []string{"a"}, ts); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got, _ := mr.Get(CounterKey(host, "a")); got != "0" {
		t.Errorf("counter = %q, want 0", got)
	}
	if mr.Exists(HeartbeatKey(host, "a")) {
		t.Error("Init must not create the heartbeat key when it is disabled")
	}

	if _, err := st.Incr(ctx, "a", 4); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if err := st.SetHeartbeat(ctx, "a", 4, ts); err != nil {
		t.Fatalf("SetHeartbeat must be a silent no-op: %v", err)
	}
	if mr.Exists(HeartbeatKey(host, "a")) {
		t.Error("SetHeartbeat must not write the key when it is disabled")
	}

	if err := st.Reset(ctx, "a", ts); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got, _ := mr.Get(CounterKey(host, "a")); got != "0" {
		t.Errorf("counter after reset = %q, want 0", got)
	}
	if mr.Exists(HeartbeatKey(host, "a")) {
		t.Error("Reset must not write the heartbeat key when it is disabled")
	}

	// Only the counter key exists, nothing else was created along the way.
	if got := mr.Keys(); len(got) != 1 || got[0] != CounterKey(host, "a") {
		t.Errorf("keys = %v, want only %q", got, CounterKey(host, "a"))
	}
}

// A pre-existing heartbeat key is left alone rather than deleted: turning the
// option off stops the updates, cleaning up is the operator's call.
func TestHeartbeatDisabledLeavesAnExistingKeyUntouched(t *testing.T) {
	st, mr := newStoreWithHeartbeat(t, false)
	ctx := context.Background()

	stale := FormatHeartbeat(host, ts, "a", 7)
	if err := mr.Set(HeartbeatKey(host, "a"), stale); err != nil {
		t.Fatal(err)
	}

	if err := st.Init(ctx, []string{"a"}, ts); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := st.Incr(ctx, "a", 1); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if err := st.Reset(ctx, "a", ts); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if got, _ := mr.Get(HeartbeatKey(host, "a")); got != stale {
		t.Errorf("heartbeat = %q, want it untouched (%q)", got, stale)
	}
}

func TestIncrAndSetValue(t *testing.T) {
	st, mr := newStore(t)
	ctx := context.Background()

	n, err := st.Incr(ctx, "get-sms", 3)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if n != 3 {
		t.Fatalf("Incr returned %d, want 3", n)
	}
	if err := st.SetHeartbeat(ctx, "get-sms", n, ts); err != nil {
		t.Fatalf("SetHeartbeat: %v", err)
	}

	// The next batch must build lines=<N> from the new INCRBY reply.
	n, err = st.Incr(ctx, "get-sms", 4)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if n != 7 {
		t.Fatalf("Incr returned %d, want 7", n)
	}
	if err := st.SetHeartbeat(ctx, "get-sms", n, ts); err != nil {
		t.Fatalf("SetHeartbeat: %v", err)
	}

	if got, _ := mr.Get(CounterKey(host, "get-sms")); got != "7" {
		t.Errorf("counter = %q, want 7", got)
	}
	got, _ := mr.Get(HeartbeatKey(host, "get-sms"))
	want := "server=web01 time=2026-08-18T15:04:05+03:00 type=get-sms lines=7"
	if got != want {
		t.Errorf("value = %q, want %q", got, want)
	}

	if v, err := st.Get(ctx, "get-sms"); err != nil || v != 7 {
		t.Errorf("Get = %d, %v; want 7, nil", v, err)
	}
}

// Counters is the read side used by the metrics exporter: one round trip for
// every action, and a key that does not exist is simply absent from the result.
func TestCounters(t *testing.T) {
	st, mr := newStore(t)
	ctx := context.Background()

	actions := []string{"get-number", "get-sms", "getStatus"}
	if _, err := st.Incr(ctx, "get-number", 7); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if err := mr.Set(CounterKey(host, "get-sms"), "0"); err != nil {
		t.Fatal(err)
	}

	got, err := st.Counters(ctx, actions)
	if err != nil {
		t.Fatalf("Counters: %v", err)
	}
	want := map[string]int64{"get-number": 7, "get-sms": 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Counters = %v, want %v: a missing key is a missing entry, not a zero", got, want)
	}

	// An empty action list is not a round trip at all.
	if got, err := st.Counters(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("Counters(nil) = %v, %v; want an empty result and no error", got, err)
	}

	// A value that is not an integer belongs to something else with the same
	// key; report it instead of silently exporting a wrong number.
	if err := mr.Set(CounterKey(host, "getStatus"), "not-a-number"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Counters(ctx, actions); err == nil {
		t.Error("Counters must fail on a value that is not an integer")
	}
}

func TestCountersFailWhenRedisIsDown(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits for the go-redis dial retries")
	}
	st, mr := newStore(t)
	ctx := context.Background()

	if _, err := st.Counters(ctx, []string{"a"}); err != nil {
		t.Fatalf("Counters: %v", err)
	}
	mr.Close()

	if _, err := st.Counters(ctx, []string{"a"}); err == nil {
		t.Error("Counters must fail once Redis is gone")
	}
}

func TestReset(t *testing.T) {
	st, mr := newStore(t)
	ctx := context.Background()

	if _, err := st.Incr(ctx, "getStatus", 99); err != nil {
		t.Fatal(err)
	}
	if err := st.SetHeartbeat(ctx, "getStatus", 99, ts); err != nil {
		t.Fatal(err)
	}

	if err := st.Reset(ctx, "getStatus", ts); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got, _ := mr.Get(CounterKey(host, "getStatus")); got != "0" {
		t.Errorf("counter after reset = %q, want 0", got)
	}
	got, _ := mr.Get(HeartbeatKey(host, "getStatus"))
	if !strings.HasSuffix(got, "lines=0") {
		t.Errorf("value after reset = %q, want lines=0", got)
	}

	// Counting resumes from zero.
	n, err := st.Incr(ctx, "getStatus", 1)
	if err != nil || n != 1 {
		t.Fatalf("Incr after reset = %d, %v; want 1, nil", n, err)
	}
}

func TestPingAndErrorsWhenRedisIsDown(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits for the go-redis dial retries")
	}
	st, mr := newStore(t)
	ctx := context.Background()

	if err := st.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	mr.Close()

	if err := st.Ping(ctx); err == nil {
		t.Error("Ping must fail once Redis is gone")
	}
	if _, err := st.Incr(ctx, "a", 1); err == nil {
		t.Error("Incr must fail once Redis is gone")
	}
	if err := st.SetHeartbeat(ctx, "a", 1, ts); err == nil {
		t.Error("SetHeartbeat must fail once Redis is gone")
	}
	if err := st.Reset(ctx, "a", ts); err == nil {
		t.Error("Reset must fail once Redis is gone")
	}
	if err := st.Init(ctx, []string{"a"}, ts); err == nil {
		t.Error("Init must fail once Redis is gone")
	}
	if _, err := st.Get(ctx, "a"); err == nil {
		t.Error("Get must fail once Redis is gone")
	}
}

func TestNewUsesConfiguredAddress(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.Select(3)
	h, p, err := net.SplitHostPort(mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}

	st := New(config.Redis{Host: h, Port: port, DB: 3, TTL: 3600}, host, true)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if err := st.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := st.Incr(ctx, "a", 5); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if got, _ := mr.Get(CounterKey(host, "a")); got != "5" {
		t.Fatalf("counter in db 3 = %q, want 5", got)
	}
}

func TestIsUnavailable(t *testing.T) {
	st, mr := newStore(t)
	ctx := context.Background()

	if IsUnavailable(nil) {
		t.Error("IsUnavailable(nil) must be false")
	}

	// A reply from the server means the connection is fine, even on an error.
	if err := mr.Set("logstat:counter:web01:a", "not-a-number"); err != nil {
		t.Fatal(err)
	}
	_, err := st.Incr(ctx, "a", 1)
	if err == nil {
		t.Fatal("INCRBY on a non-integer value must fail")
	}
	if IsUnavailable(err) {
		t.Errorf("a server reply error must not count as unavailable: %v", err)
	}

	if _, err := st.Get(ctx, "missing"); err == nil {
		t.Fatal("GET of a missing key must fail")
	} else if IsUnavailable(err) {
		t.Errorf("redis.Nil must not count as unavailable: %v", err)
	}

	// A dead server does.
	mr.Close()
	if _, err := st.Incr(ctx, "b", 1); err == nil {
		t.Fatal("expected a dial error")
	} else if !IsUnavailable(err) {
		t.Errorf("a dial error must count as unavailable: %v", err)
	}
}

func TestSetLibraryLogger(t *testing.T) {
	buf := &lockedBuffer{}
	lg := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	SetLibraryLogger(lg)
	t.Cleanup(func() { SetLibraryLogger(slog.New(slog.NewTextHandler(io.Discard, nil))) })

	// Driving the adapter directly keeps the test fast: reproducing a real pool
	// failure would only add dial timeouts, and the point is that the message is
	// routed and tagged instead of going to stderr.
	libraryLogger{lg: lg}.Printf(context.Background(), "failed to dial after %d attempts\n", 5)

	out := buf.String()
	if !strings.Contains(out, "failed to dial after 5 attempts") {
		t.Errorf("library message not routed: %q", out)
	}
	if !strings.Contains(out, "component=go-redis") {
		t.Errorf("library message not tagged: %q", out)
	}
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("library message must be logged at debug level: %q", out)
	}
	if strings.Contains(out, `attempts\n`) {
		t.Errorf("trailing newline not trimmed: %q", out)
	}
}

// lockedBuffer is a bytes.Buffer safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const day = 24 * time.Hour

func TestTTLIsAppliedToEveryKey(t *testing.T) {
	st, mr := newTestStore(t, true, day)
	ctx := context.Background()

	if st.TTL() != day {
		t.Fatalf("TTL = %s, want %s", st.TTL(), day)
	}

	if err := st.Init(ctx, []string{"a"}, ts); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, k := range []string{CounterKey(host, "a"), HeartbeatKey(host, "a")} {
		if got := mr.TTL(k); got != day {
			t.Errorf("TTL of %s after Init = %s, want %s", k, got, day)
		}
	}

	// INCRBY does not renew an expiry by itself, so the store has to.
	mr.FastForward(12 * time.Hour)
	if _, err := st.Incr(ctx, "a", 1); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if got := mr.TTL(CounterKey(host, "a")); got != day {
		t.Errorf("TTL after Incr = %s, want it refreshed to %s", got, day)
	}

	if err := st.SetHeartbeat(ctx, "a", 1, ts); err != nil {
		t.Fatalf("SetHeartbeat: %v", err)
	}
	if got := mr.TTL(HeartbeatKey(host, "a")); got != day {
		t.Errorf("TTL after SetHeartbeat = %s, want %s", got, day)
	}

	if err := st.Reset(ctx, "a", ts); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, k := range []string{CounterKey(host, "a"), HeartbeatKey(host, "a")} {
		if got := mr.TTL(k); got != day {
			t.Errorf("TTL of %s after Reset = %s, want %s", k, got, day)
		}
	}
}

func TestTouchRefreshesTTL(t *testing.T) {
	st, mr := newTestStore(t, true, day)
	ctx := context.Background()
	actions := []string{"a", "b"}

	if err := st.Init(ctx, actions, ts); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(20 * time.Hour)
	for _, k := range []string{CounterKey(host, "a"), HeartbeatKey(host, "b")} {
		if got := mr.TTL(k); got != 4*time.Hour {
			t.Fatalf("TTL of %s = %s, want 4h before the refresh", k, got)
		}
	}

	if err := st.Touch(ctx, actions); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	for _, a := range actions {
		for _, k := range []string{CounterKey(host, a), HeartbeatKey(host, a)} {
			if got := mr.TTL(k); got != day {
				t.Errorf("TTL of %s after Touch = %s, want %s", k, got, day)
			}
		}
	}
}

// The counter of an idle action would silently lose its total if nothing kept it
// alive, so an expiry that is never refreshed is exactly what must not happen.
func TestKeysExpireOnlyWithoutARefresh(t *testing.T) {
	st, mr := newTestStore(t, true, day)
	ctx := context.Background()

	if err := st.Init(ctx, []string{"a"}, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Incr(ctx, "a", 7); err != nil {
		t.Fatal(err)
	}

	// Refreshed in time: the total is still there.
	mr.FastForward(23 * time.Hour)
	if err := st.Touch(ctx, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(23 * time.Hour)
	if got, _ := mr.Get(CounterKey(host, "a")); got != "7" {
		t.Fatalf("counter = %q, want 7: a refreshed key must not expire", got)
	}

	// Left alone past the TTL: gone, which is how a dead instance is cleaned up.
	mr.FastForward(2 * time.Hour)
	if mr.Exists(CounterKey(host, "a")) || mr.Exists(HeartbeatKey(host, "a")) {
		t.Errorf("keys = %v, want them expired", mr.Keys())
	}
}

func TestTTLZeroLeavesKeysForever(t *testing.T) {
	st, mr := newTestStore(t, true, 0)
	ctx := context.Background()

	if err := st.Init(ctx, []string{"a"}, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Incr(ctx, "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetHeartbeat(ctx, "a", 1, ts); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{CounterKey(host, "a"), HeartbeatKey(host, "a")} {
		if got := mr.TTL(k); got != 0 {
			t.Errorf("TTL of %s = %s, want no expiry", k, got)
		}
	}
	mr.FastForward(365 * day)
	if !mr.Exists(CounterKey(host, "a")) {
		t.Error("a key without an expiry must survive")
	}
}

// Turning the option off has to be reversible: Touch clears an expiry that an
// earlier configuration left behind.
func TestTouchWithTTLZeroPersistsExistingKeys(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	expiring := NewWithClient(rdb, host, true, day)
	if err := expiring.Init(ctx, []string{"a"}, ts); err != nil {
		t.Fatal(err)
	}
	if got := mr.TTL(CounterKey(host, "a")); got != day {
		t.Fatalf("TTL = %s, want %s", got, day)
	}

	persisting := NewWithClient(rdb, host, true, 0)
	if err := persisting.Touch(ctx, []string{"a"}); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	for _, k := range []string{CounterKey(host, "a"), HeartbeatKey(host, "a")} {
		if got := mr.TTL(k); got != 0 {
			t.Errorf("TTL of %s = %s, want it cleared", k, got)
		}
	}
	mr.FastForward(2 * day)
	if !mr.Exists(CounterKey(host, "a")) {
		t.Error("the key must survive after the expiry was cleared")
	}
}

func TestTouchSkipsTheHeartbeatWhenDisabled(t *testing.T) {
	st, mr := newTestStore(t, false, day)
	ctx := context.Background()

	if err := mr.Set(HeartbeatKey(host, "a"), "stale"); err != nil {
		t.Fatal(err)
	}
	if err := st.Init(ctx, []string{"a"}, ts); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(ctx, []string{"a"}); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if got := mr.TTL(CounterKey(host, "a")); got != day {
		t.Errorf("counter TTL = %s, want %s", got, day)
	}
	if got := mr.TTL(HeartbeatKey(host, "a")); got != 0 {
		t.Errorf("a disabled heartbeat must not be touched, TTL = %s", got)
	}
}

func TestTouchAndTTLErrorsWhenRedisIsDown(t *testing.T) {
	st, mr := newTestStore(t, true, day)
	ctx := context.Background()
	mr.Close()

	if err := st.Touch(ctx, []string{"a"}); err == nil {
		t.Error("Touch must fail once Redis is gone")
	} else if !IsUnavailable(err) {
		t.Errorf("the error must count as unavailable: %v", err)
	}
	if _, err := st.Incr(ctx, "a", 1); err == nil {
		t.Error("Incr must fail once Redis is gone")
	}
	if err := st.Touch(ctx, nil); err != nil {
		t.Errorf("Touch with no actions must be a no-op, got %v", err)
	}
}
