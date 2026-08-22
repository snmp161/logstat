package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"

	"github.com/snmp161/logstat/internal/config"
	"github.com/snmp161/logstat/internal/counter"
	"github.com/snmp161/logstat/internal/store"
)

const testHost = "web01"

var (
	start = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// The clock the collector sees, 90 seconds after the start.
	scrapeTime = start.Add(90 * time.Second)
)

// stubReader stands in for the store when the test needs to control what the
// Redis read returns, an outage in particular.
type stubReader struct {
	counters map[string]int64
	err      error
	calls    int
}

func (s *stubReader) Host() string { return testHost }

func (s *stubReader) Counters(_ context.Context, _ []string) (map[string]int64, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.counters, nil
}

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.Actions = []string{"get-number", "get-sms"}
	cfg.LogPath = "/var/log/app.log"
	cfg.LockFile = "/run/logstat/default/logstat.lock"
	cfg.Redis.Host = "10.0.0.5"
	cfg.Redis.Port = 6380
	cfg.Redis.DB = 3
	cfg.Redis.Password = "hunter2"
	cfg.Metrics.Enabled = true
	cfg.Metrics.Listen = "127.0.0.1:9843"
	cfg.Metrics.Path = "/metrics"
	return &cfg
}

func newTestCollector(t *testing.T, cfg *config.Config, cnt *counter.Counter, rd Reader) *Collector {
	t.Helper()
	return NewCollector(cfg, cnt, rd, Options{
		Start:   start,
		Now:     func() time.Time { return scrapeTime },
		Version: "v9.9.9-test",
	})
}

// gather renders the collector into a family-by-name map, the way a scrape
// would see it.
func gather(t *testing.T, c prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

// value returns the single sample of a metric without labels.
func value(t *testing.T, families map[string]*dto.MetricFamily, name string) float64 {
	t.Helper()
	f, ok := families[name]
	if !ok {
		t.Fatalf("metric %s is missing, got %v", name, names(families))
	}
	if len(f.GetMetric()) != 1 {
		t.Fatalf("metric %s has %d series, want 1", name, len(f.GetMetric()))
	}
	return sample(f.GetMetric()[0])
}

// labelled returns the sample carrying label=want, or fails.
func labelled(t *testing.T, families map[string]*dto.MetricFamily, name, label, want string) float64 {
	t.Helper()
	f, ok := families[name]
	if !ok {
		t.Fatalf("metric %s is missing, got %v", name, names(families))
	}
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == label && l.GetValue() == want {
				return sample(m)
			}
		}
	}
	t.Fatalf("metric %s has no series with %s=%q", name, label, want)
	return 0
}

// has reports whether the family carries a series with label=want.
func has(families map[string]*dto.MetricFamily, name, label, want string) bool {
	f, ok := families[name]
	if !ok {
		return false
	}
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == label && l.GetValue() == want {
				return true
			}
		}
	}
	return false
}

func sample(m *dto.Metric) float64 {
	switch {
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetUntyped() != nil:
		return m.GetUntyped().GetValue()
	}
	return 0
}

func names(families map[string]*dto.MetricFamily) []string {
	out := make([]string, 0, len(families))
	for n := range families {
		out = append(out, n)
	}
	return out
}

func labels(t *testing.T, families map[string]*dto.MetricFamily, name string) map[string]string {
	t.Helper()
	f, ok := families[name]
	if !ok {
		t.Fatalf("metric %s is missing, got %v", name, names(families))
	}
	if len(f.GetMetric()) != 1 {
		t.Fatalf("metric %s has %d series, want 1", name, len(f.GetMetric()))
	}
	out := map[string]string{}
	for _, l := range f.GetMetric()[0].GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

func TestCollectorUptimeAndStartTime(t *testing.T) {
	c := newTestCollector(t, testConfig(), counter.New([]string{"a"}, true), &stubReader{})
	families := gather(t, c)

	if got := value(t, families, "logstat_uptime_seconds"); got != 90 {
		t.Errorf("logstat_uptime_seconds = %v, want 90", got)
	}
	if got := value(t, families, "logstat_start_time_seconds"); got != float64(start.Unix()) {
		t.Errorf("logstat_start_time_seconds = %v, want %v", got, float64(start.Unix()))
	}
}

// Without an injected clock the collector uses the real one, so the uptime of a
// process that just started is small but not negative.
func TestCollectorUsesTheRealClockByDefault(t *testing.T) {
	c := NewCollector(testConfig(), counter.New([]string{"a"}, true), &stubReader{}, Options{Start: time.Now()})
	got := value(t, gather(t, c), "logstat_uptime_seconds")
	if got < 0 || got > 60 {
		t.Fatalf("logstat_uptime_seconds = %v, want a small non-negative number", got)
	}
}

func TestCollectorConfigMetrics(t *testing.T) {
	cfg := testConfig()
	cfg.FlushInterval = 7
	cfg.Redis.TTL = 3600
	cfg.CaseSensitive = false
	cfg.Poll = true
	cfg.HeartbeatKey = false
	cfg.Reset.Enabled = false
	cfg.Reset.Schedule = "*/30 * * * *"
	cfg.Logging.Level = "debug"
	cfg.Logging.Output = config.OutputFile
	cfg.Logging.File = "/var/log/logstat/app.log"

	families := gather(t, newTestCollector(t, cfg, counter.New(cfg.Actions, cfg.CaseSensitive), &stubReader{}))

	numeric := map[string]float64{
		"logstat_config_flush_interval_seconds": 7,
		"logstat_config_redis_ttl_seconds":      3600,
		"logstat_config_case_sensitive":         0,
		"logstat_config_poll":                   1,
		"logstat_config_heartbeat_key":          0,
		"logstat_config_reset_enabled":          0,
		"logstat_config_actions":                2,
	}
	for name, want := range numeric {
		if got := value(t, families, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	if got := value(t, families, "logstat_config_info"); got != 1 {
		t.Errorf("logstat_config_info = %v, want 1", got)
	}
	want := map[string]string{
		"log_path":           "/var/log/app.log",
		"lock_file":          "/run/logstat/default/logstat.lock",
		"host":               testHost,
		"redis_addr":         "10.0.0.5:6380",
		"redis_db":           "3",
		"redis_password_set": "true",
		"logging_level":      "debug",
		"logging_output":     "file",
		"logging_file":       "/var/log/logstat/app.log",
		"reset_schedule":     "*/30 * * * *",
		"metrics_listen":     "127.0.0.1:9843",
		"metrics_path":       "/metrics",
	}
	got := labels(t, families, "logstat_config_info")
	for k, v := range want {
		if got[k] != v {
			t.Errorf("logstat_config_info label %s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("logstat_config_info labels = %v, want exactly %v", got, want)
	}
}

// The password is the one config value that must never leave the process, in
// any label of any metric.
func TestCollectorNeverExposesTheRedisPassword(t *testing.T) {
	cfg := testConfig()
	cfg.Redis.Password = "s3cret-p4ssword"

	families := gather(t, newTestCollector(t, cfg, counter.New(cfg.Actions, true), &stubReader{}))
	for name, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.Contains(l.GetValue(), cfg.Redis.Password) {
					t.Fatalf("metric %s exposes the Redis password in label %s", name, l.GetName())
				}
			}
		}
	}

	if got := labels(t, families, "logstat_config_info")["redis_password_set"]; got != "true" {
		t.Errorf("redis_password_set = %q, want \"true\"", got)
	}

	cfg.Redis.Password = ""
	families = gather(t, newTestCollector(t, cfg, counter.New(cfg.Actions, true), &stubReader{}))
	if got := labels(t, families, "logstat_config_info")["redis_password_set"]; got != "false" {
		t.Errorf("redis_password_set without a password = %q, want \"false\"", got)
	}
}

// Every configured word is exported from the start, at zero, and the totals
// survive the flush that empties the buffer.
func TestCollectorPerActionSeries(t *testing.T) {
	cfg := testConfig()
	cnt := counter.New(cfg.Actions, true)
	c := newTestCollector(t, cfg, cnt, &stubReader{})

	families := gather(t, c)
	for _, action := range cfg.Actions {
		if got := labelled(t, families, "logstat_matched_lines_total", "action", action); got != 0 {
			t.Errorf("logstat_matched_lines_total{action=%q} = %v, want 0", action, got)
		}
		if got := labelled(t, families, "logstat_pending_increments", "action", action); got != 0 {
			t.Errorf("logstat_pending_increments{action=%q} = %v, want 0", action, got)
		}
	}
	if got := value(t, families, "logstat_lines_read_total"); got != 0 {
		t.Errorf("logstat_lines_read_total = %v, want 0", got)
	}

	cnt.ProcessLine("get-number")
	cnt.ProcessLine("get-number and get-sms")
	cnt.ProcessLine("neither of them")

	families = gather(t, c)
	if got := labelled(t, families, "logstat_matched_lines_total", "action", "get-number"); got != 2 {
		t.Errorf("matched{get-number} = %v, want 2", got)
	}
	if got := labelled(t, families, "logstat_pending_increments", "action", "get-sms"); got != 1 {
		t.Errorf("pending{get-sms} = %v, want 1", got)
	}
	if got := value(t, families, "logstat_lines_read_total"); got != 3 {
		t.Errorf("logstat_lines_read_total = %v, want 3", got)
	}

	// The flush empties the buffer; the counter of the process keeps its total,
	// or every flush would look like a restart to Prometheus.
	cnt.Drain()
	families = gather(t, c)
	if got := labelled(t, families, "logstat_matched_lines_total", "action", "get-number"); got != 2 {
		t.Errorf("matched{get-number} after a flush = %v, want 2", got)
	}
	if got := labelled(t, families, "logstat_pending_increments", "action", "get-number"); got != 0 {
		t.Errorf("pending{get-number} after a flush = %v, want 0", got)
	}
}

// The names, types and help strings are part of the interface with the
// monitoring side, so they are pinned down here.
func TestCollectorExposition(t *testing.T) {
	cfg := testConfig()
	cfg.Actions = []string{"get-number"}
	cnt := counter.New(cfg.Actions, true)
	cnt.ProcessLine("get-number")

	c := newTestCollector(t, cfg, cnt, &stubReader{counters: map[string]int64{"get-number": 41}})

	const want = `
# HELP logstat_matched_lines_total Log lines matching the code word since the daemon started.
# TYPE logstat_matched_lines_total counter
logstat_matched_lines_total{action="get-number"} 1
# HELP logstat_pending_increments Increments buffered in memory, not yet flushed to Redis.
# TYPE logstat_pending_increments gauge
logstat_pending_increments{action="get-number"} 1
# HELP logstat_redis_counter Current value of the integer counter in Redis.
# TYPE logstat_redis_counter gauge
logstat_redis_counter{action="get-number"} 41
# HELP logstat_uptime_seconds Seconds since the daemon started.
# TYPE logstat_uptime_seconds gauge
logstat_uptime_seconds 90
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"logstat_matched_lines_total", "logstat_pending_increments",
		"logstat_redis_counter", "logstat_uptime_seconds"); err != nil {
		t.Fatal(err)
	}
}

// The Redis values are read during the scrape, so they follow whatever is in
// Redis right now — including a key that disappeared.
func TestCollectorReadsRedisOnEveryScrape(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := store.NewWithClient(rdb, testHost, true, 0)

	cfg := testConfig()
	c := newTestCollector(t, cfg, counter.New(cfg.Actions, true), st)

	if err := mr.Set(store.CounterKey(testHost, "get-number"), "8125"); err != nil {
		t.Fatal(err)
	}

	families := gather(t, c)
	if got := labelled(t, families, "logstat_redis_counter", "action", "get-number"); got != 8125 {
		t.Errorf("logstat_redis_counter{get-number} = %v, want 8125", got)
	}
	// The key of get-sms does not exist: a missing key is a missing series, not
	// a zero that nobody can tell from a counter that was really zeroed.
	if has(families, "logstat_redis_counter", "action", "get-sms") {
		t.Error("logstat_redis_counter{get-sms} exists although the key does not")
	}
	if got := value(t, families, "logstat_redis_up"); got != 1 {
		t.Errorf("logstat_redis_up = %v, want 1", got)
	}
	if got := value(t, families, "logstat_redis_scrape_errors_total"); got != 0 {
		t.Errorf("logstat_redis_scrape_errors_total = %v, want 0", got)
	}

	// A later scrape sees the new value.
	if err := mr.Set(store.CounterKey(testHost, "get-number"), "8126"); err != nil {
		t.Fatal(err)
	}
	families = gather(t, c)
	if got := labelled(t, families, "logstat_redis_counter", "action", "get-number"); got != 8126 {
		t.Errorf("logstat_redis_counter{get-number} = %v, want 8126", got)
	}

	// The key expires or is deleted: the series goes away with it.
	mr.Del(store.CounterKey(testHost, "get-number"))
	families = gather(t, c)
	if has(families, "logstat_redis_counter", "action", "get-number") {
		t.Error("logstat_redis_counter{get-number} survived the deletion of the key")
	}
}

// A Redis outage must not break the scrape: the endpoint still answers, it just
// says so.
func TestCollectorSurvivesARedisOutage(t *testing.T) {
	cfg := testConfig()
	rd := &stubReader{err: errors.New("dial tcp: connection refused")}
	c := newTestCollector(t, cfg, counter.New(cfg.Actions, true), rd)

	families := gather(t, c)
	if got := value(t, families, "logstat_redis_up"); got != 0 {
		t.Errorf("logstat_redis_up = %v, want 0", got)
	}
	if got := value(t, families, "logstat_redis_scrape_errors_total"); got != 1 {
		t.Errorf("logstat_redis_scrape_errors_total = %v, want 1", got)
	}
	if _, ok := families["logstat_redis_counter"]; ok {
		t.Error("logstat_redis_counter must be absent while Redis is unreachable")
	}
	// Everything that does not need Redis is still exported.
	if got := value(t, families, "logstat_uptime_seconds"); got != 90 {
		t.Errorf("logstat_uptime_seconds = %v, want 90", got)
	}

	families = gather(t, c)
	if got := value(t, families, "logstat_redis_scrape_errors_total"); got != 2 {
		t.Errorf("logstat_redis_scrape_errors_total after a second failed scrape = %v, want 2", got)
	}

	// Recovery needs no restart.
	rd.err = nil
	rd.counters = map[string]int64{"get-sms": 5}
	families = gather(t, c)
	if got := value(t, families, "logstat_redis_up"); got != 1 {
		t.Errorf("logstat_redis_up after the recovery = %v, want 1", got)
	}
	if got := labelled(t, families, "logstat_redis_counter", "action", "get-sms"); got != 5 {
		t.Errorf("logstat_redis_counter{get-sms} = %v, want 5", got)
	}
	if got := value(t, families, "logstat_redis_scrape_errors_total"); got != 2 {
		t.Errorf("logstat_redis_scrape_errors_total must not grow on a good scrape, got %v", got)
	}
}

// The registry the daemon serves carries the standard collectors too.
func TestNewRegistryIncludesTheStandardCollectors(t *testing.T) {
	cfg := testConfig()
	reg := NewRegistry(newTestCollector(t, cfg, counter.New(cfg.Actions, true), &stubReader{}))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, name := range []string{"logstat_uptime_seconds", "go_goroutines"} {
		if !seen[name] {
			t.Errorf("metric %s is missing from the registry", name)
		}
	}
}
