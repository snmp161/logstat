// Package metrics exposes the state of the daemon in the Prometheus exposition
// format: its uptime, the configuration it runs with, the per-word counters of
// this process and the per-word totals stored in Redis.
package metrics

import (
	"context"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/snmp161/logstat/internal/config"
	"github.com/snmp161/logstat/internal/counter"
)

// namespace prefixes every metric of this exporter.
const namespace = "logstat_"

// defaultTimeout bounds the Redis read of a single scrape. It is deliberately
// well below the usual Prometheus scrape_timeout of 10 seconds: a scrape may
// report an unreachable Redis, but it must never hang on it.
const defaultTimeout = 2 * time.Second

// Reader is the part of the store the collector needs: the counters as Redis
// has them right now, plus the host name the keys are built from.
type Reader interface {
	Counters(ctx context.Context, actions []string) (map[string]int64, error)
	Host() string
}

// Options carries what the collector cannot derive from the configuration. The
// zero value is usable: the clock is the real one and the timeout the default.
type Options struct {
	// Start is when the process came up; it backs the uptime metric.
	Start time.Time
	// Now overrides the clock, for tests.
	Now func() time.Time
	// Timeout bounds the Redis read of one scrape.
	Timeout time.Duration
	// Version is shown on the status page. It is not a metric: build data
	// belongs to the release, not to the time series.
	Version string
	// Log receives the errors of the status page, which cannot be reported to
	// the browser once the response has started. Defaults to a discarding one.
	Log *slog.Logger
}

// Collector renders the daemon state on every scrape. The values that live in
// this process are read from the counter, the ones that live in Redis are
// fetched during the scrape itself, so a stale export cannot happen.
type Collector struct {
	cfg     *config.Config
	cnt     *counter.Counter
	reader  Reader
	start   time.Time
	now     func() time.Time
	timeout time.Duration
	version string
	log     *slog.Logger

	// redisErrors counts scrapes that could not read Redis. It is only touched
	// during Collect, which Prometheus may call concurrently.
	redisErrors atomic.Uint64

	uptime          *prometheus.Desc
	startTime       *prometheus.Desc
	configInfo      *prometheus.Desc
	flushInterval   *prometheus.Desc
	redisTTL        *prometheus.Desc
	caseSensitive   *prometheus.Desc
	poll            *prometheus.Desc
	heartbeatKey    *prometheus.Desc
	resetEnabled    *prometheus.Desc
	actions         *prometheus.Desc
	linesRead       *prometheus.Desc
	matchedLines    *prometheus.Desc
	pending         *prometheus.Desc
	redisCounter    *prometheus.Desc
	redisUp         *prometheus.Desc
	redisScrapeErrs *prometheus.Desc
}

// configInfoLabels are the labels of logstat_config_info, in a fixed order. The
// Redis password is not among them and never will be: the label only tells
// whether one is configured.
var configInfoLabels = []string{
	"log_path", "lock_file", "host",
	"redis_addr", "redis_db", "redis_password_set",
	"logging_level", "logging_output", "logging_file",
	"reset_schedule", "metrics_listen", "metrics_path",
}

// NewCollector builds the collector for one daemon.
func NewCollector(cfg *config.Config, cnt *counter.Counter, rd Reader, opts Options) *Collector {
	c := &Collector{
		cfg:     cfg,
		cnt:     cnt,
		reader:  rd,
		start:   opts.Start,
		now:     opts.Now,
		timeout: opts.Timeout,
		version: opts.Version,
		log:     opts.Log,
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.version == "" {
		c.version = "dev"
	}
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	if c.start.IsZero() {
		c.start = c.now()
	}

	gauge := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(namespace+name, help, labels, nil)
	}
	c.uptime = gauge("uptime_seconds", "Seconds since the daemon started.")
	c.startTime = gauge("start_time_seconds", "Unix timestamp of the daemon start.")
	c.configInfo = gauge("config_info", "Configuration the daemon runs with, without the Redis password.", configInfoLabels...)
	c.flushInterval = gauge("config_flush_interval_seconds", "Configured flush_interval.")
	c.redisTTL = gauge("config_redis_ttl_seconds", "Configured redis.ttl, 0 meaning no expiry.")
	c.caseSensitive = gauge("config_case_sensitive", "1 if the matching is case sensitive.")
	c.poll = gauge("config_poll", "1 if the log file is polled instead of watched with inotify.")
	c.heartbeatKey = gauge("config_heartbeat_key", "1 if the heartbeat key is maintained.")
	c.resetEnabled = gauge("config_reset_enabled", "1 if the scheduled self-reset is enabled.")
	c.actions = gauge("config_actions", "Number of configured code words.")
	c.linesRead = gauge("lines_read_total", "Log lines read since the daemon started.")
	c.matchedLines = gauge("matched_lines_total", "Log lines matching the code word since the daemon started.", "action")
	c.pending = gauge("pending_increments", "Increments buffered in memory, not yet flushed to Redis.", "action")
	c.redisCounter = gauge("redis_counter", "Current value of the integer counter in Redis.", "action")
	c.redisUp = gauge("redis_up", "1 if the last scrape could read the counters from Redis.")
	c.redisScrapeErrs = gauge("redis_scrape_errors_total", "Scrapes that failed to read the counters from Redis.")
	return c
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.uptime, c.startTime, c.configInfo, c.flushInterval, c.redisTTL,
		c.caseSensitive, c.poll, c.heartbeatKey, c.resetEnabled, c.actions,
		c.linesRead, c.matchedLines, c.pending, c.redisCounter,
		c.redisUp, c.redisScrapeErrs,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector. It is called once per scrape.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.collectProcess(ch)
	c.collectConfig(ch)
	c.collectCounter(ch)
	c.collectRedis(ch)
}

func (c *Collector) collectProcess(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.uptime, prometheus.GaugeValue, c.now().Sub(c.start).Seconds())
	ch <- prometheus.MustNewConstMetric(c.startTime, prometheus.GaugeValue, float64(c.start.Unix()))
}

func (c *Collector) collectConfig(ch chan<- prometheus.Metric) {
	cfg := c.cfg
	ch <- prometheus.MustNewConstMetric(c.configInfo, prometheus.GaugeValue, 1,
		cfg.LogPath,
		cfg.LockFile,
		c.reader.Host(),
		cfg.Redis.Addr(),
		strconv.Itoa(cfg.Redis.DB),
		boolLabel(cfg.Redis.Password != ""),
		cfg.Logging.Level,
		cfg.Logging.Output,
		cfg.Logging.File,
		cfg.Reset.Schedule,
		cfg.Metrics.Listen,
		cfg.Metrics.Path,
	)
	ch <- prometheus.MustNewConstMetric(c.flushInterval, prometheus.GaugeValue, float64(cfg.FlushInterval))
	ch <- prometheus.MustNewConstMetric(c.redisTTL, prometheus.GaugeValue, float64(cfg.Redis.TTL))
	ch <- prometheus.MustNewConstMetric(c.caseSensitive, prometheus.GaugeValue, boolValue(cfg.CaseSensitive))
	ch <- prometheus.MustNewConstMetric(c.poll, prometheus.GaugeValue, boolValue(cfg.Poll))
	ch <- prometheus.MustNewConstMetric(c.heartbeatKey, prometheus.GaugeValue, boolValue(cfg.HeartbeatKey))
	ch <- prometheus.MustNewConstMetric(c.resetEnabled, prometheus.GaugeValue, boolValue(cfg.Reset.Enabled))
	ch <- prometheus.MustNewConstMetric(c.actions, prometheus.GaugeValue, float64(len(cfg.Actions)))
}

func (c *Collector) collectCounter(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.linesRead, prometheus.CounterValue, float64(c.cnt.Lines()))
	for action, n := range c.cnt.Matched() {
		ch <- prometheus.MustNewConstMetric(c.matchedLines, prometheus.CounterValue, float64(n), action)
	}
	for action, n := range c.cnt.PendingByAction() {
		ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(n), action)
	}
}

// collectRedis reads the shared totals during the scrape. A failure is reported
// as data (redis_up 0 plus an error counter) instead of failing the scrape: the
// metrics that have nothing to do with Redis must stay readable during an outage.
func (c *Collector) collectRedis(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	counters, err := c.reader.Counters(ctx, c.cfg.Actions)
	if err != nil {
		c.redisErrors.Add(1)
		ch <- prometheus.MustNewConstMetric(c.redisUp, prometheus.GaugeValue, 0)
	} else {
		ch <- prometheus.MustNewConstMetric(c.redisUp, prometheus.GaugeValue, 1)
		// A key that does not exist is missing from the map and stays missing
		// here: an invented zero is indistinguishable from a real one.
		for action, n := range counters {
			ch <- prometheus.MustNewConstMetric(c.redisCounter, prometheus.GaugeValue, float64(n), action)
		}
	}
	ch <- prometheus.MustNewConstMetric(c.redisScrapeErrs, prometheus.CounterValue, float64(c.redisErrors.Load()))
}

// NewRegistry returns the registry the exporter serves: the daemon's own
// collector plus the standard go_* and process_* ones.
func NewRegistry(c *Collector) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		c,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
