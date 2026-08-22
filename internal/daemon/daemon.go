// Package daemon wires the log tailer, the in-memory counter, the periodic
// flush to Redis and the optional scheduled reset into one process.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nxadm/tail"
	"github.com/robfig/cron/v3"

	"github.com/snmp161/logstat/internal/config"
	"github.com/snmp161/logstat/internal/counter"
	"github.com/snmp161/logstat/internal/metrics"
	"github.com/snmp161/logstat/internal/store"
)

// opTimeout bounds a single Redis round trip. It is deliberately independent of
// the daemon context so that the final flush still runs after SIGTERM.
const opTimeout = 5 * time.Second

// Daemon is a single log file watcher. It is not safe for concurrent Run calls.
type Daemon struct {
	cfg   *config.Config
	log   *slog.Logger
	store *store.Store
	cnt   *counter.Counter

	cronOpts []cron.Option

	// initialized tracks whether the SETNX bootstrap has succeeded; it is
	// retried on every flush until Redis becomes reachable.
	initialized bool
	// pendingHeartbeats holds totals whose heartbeat value could not be written
	// after a successful INCRBY, keyed by action.
	pendingHeartbeats map[string]int64
	// pendingResets holds actions whose scheduled reset failed and must be
	// retried on the next flush.
	pendingResets map[string]bool
	// redisDown avoids logging the same connection error on every flush.
	redisDown bool

	// started is when the process came up; the exporter reports it as uptime.
	started time.Time
	// version is shown on the status page of the exporter.
	version string
	// metricsAddr is the address the exporter bound, empty while it is not
	// running. Read by tests and by anything wanting the effective port.
	metricsAddr atomic.Pointer[string]
}

// Option customises a Daemon.
type Option func(*Daemon)

// WithCronOptions overrides the options passed to the cron scheduler. Tests use
// it to enable the six-field (seconds) parser.
func WithCronOptions(opts ...cron.Option) Option {
	return func(d *Daemon) { d.cronOpts = opts }
}

// WithVersion passes the build version through to the status page of the
// metrics exporter.
func WithVersion(v string) Option {
	return func(d *Daemon) { d.version = v }
}

// New builds a daemon for cfg. The store and logger are supplied by the caller
// so that tests can inject miniredis and a buffer.
func New(cfg *config.Config, lg *slog.Logger, st *store.Store, opts ...Option) *Daemon {
	d := &Daemon{
		cfg:               cfg,
		log:               lg,
		store:             st,
		cnt:               counter.New(cfg.Actions, cfg.CaseSensitive),
		pendingHeartbeats: make(map[string]int64),
		pendingResets:     make(map[string]bool),
		started:           time.Now(),
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Counter exposes the in-memory buffer, for tests and diagnostics.
func (d *Daemon) Counter() *counter.Counter { return d.cnt }

// MetricsAddr returns the address the metrics exporter is listening on, or an
// empty string when it is disabled or not up yet.
func (d *Daemon) MetricsAddr() string {
	if addr := d.metricsAddr.Load(); addr != nil {
		return *addr
	}
	return ""
}

// Run tails the log file until ctx is cancelled, then performs a final flush.
func (d *Daemon) Run(ctx context.Context) error {
	d.log.Info("starting",
		"log_path", d.cfg.LogPath,
		"actions", len(d.cfg.Actions),
		"case_sensitive", d.cfg.CaseSensitive,
		"flush_interval", d.cfg.FlushInterval,
		"poll", d.cfg.Poll,
		"redis", d.cfg.Redis.Addr(),
		"redis_db", d.cfg.Redis.DB,
		"redis_ttl", d.cfg.Redis.TTLDuration(),
		"host", d.store.Host(),
		"heartbeat_key", d.cfg.HeartbeatKey,
		"reset_enabled", d.cfg.Reset.Enabled,
		"reset_schedule", d.cfg.Reset.Schedule,
		"metrics_enabled", d.cfg.Metrics.Enabled)

	// The exporter binds before anything is read: a port already in use has to
	// stop the daemon right away rather than after the first flush.
	stopMetrics, err := d.startMetrics()
	if err != nil {
		return err
	}
	defer stopMetrics()

	// An existing log file is opened at its end (tail -n0): what was written
	// before the daemon started is history and must not be counted. A file that
	// does not exist yet is read from its beginning once it appears, exactly like
	// a file recreated by a rotation — otherwise the first lines of the source
	// would be lost in the gap between creation and the seek.
	var location *tail.SeekInfo
	if _, err := os.Stat(d.cfg.LogPath); err == nil {
		location = &tail.SeekInfo{Offset: 0, Whence: io.SeekEnd}
		d.log.Info("log file found, tailing from its end", "log_path", d.cfg.LogPath)
	} else {
		d.log.Info("log file does not exist yet, waiting for it", "log_path", d.cfg.LogPath)
	}

	t, err := tail.TailFile(d.cfg.LogPath, tail.Config{
		Location:      location,
		ReOpen:        true,
		Follow:        true,
		MustExist:     false,
		Poll:          d.cfg.Poll,
		CompleteLines: true,
		Logger:        tailLogger{log: d.log},
	})
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.consume(t)
	}()

	// Bootstrapping the Redis keys comes after the tailer is attached: a slow or
	// unreachable Redis must not delay the seek, or the lines written meanwhile
	// would be skipped. It is retried on every flush until it succeeds.
	_ = d.ensureInit(ctx)

	ticker := time.NewTicker(time.Duration(d.cfg.FlushInterval) * time.Second)
	defer ticker.Stop()

	resetCh := make(chan time.Time, 1)
	if d.cfg.Reset.Enabled {
		c := cron.New(d.cronOpts...)
		if _, err := c.AddFunc(d.cfg.Reset.Schedule, func() {
			// Non-blocking: a reset already queued covers this tick too.
			select {
			case resetCh <- time.Now():
			default:
			}
		}); err != nil {
			_ = t.Stop()
			wg.Wait()
			return err
		}
		c.Start()
		defer func() { <-c.Stop().Done() }()
		d.log.Info("reset scheduled", "schedule", d.cfg.Reset.Schedule)
	} else {
		d.log.Info("self-reset disabled, counters are zeroed externally")
	}

	for {
		select {
		case <-ctx.Done():
			d.log.Info("shutting down, flushing buffer")
			// Stop the tailer first so that every line already read is counted.
			if err := t.Stop(); err != nil {
				d.log.Warn("tail stopped with error", "error", err)
			}
			wg.Wait()
			d.Flush(context.WithoutCancel(ctx))
			d.log.Info("stopped")
			return nil

		case <-ticker.C:
			d.Flush(ctx)

		case <-resetCh:
			d.log.Info("scheduled reset fired", "schedule", d.cfg.Reset.Schedule)
			d.Reset(ctx)
		}
	}
}

// startMetrics brings the Prometheus exporter up and returns the function that
// takes it down again. With the exporter disabled both are no-ops.
//
// Binding happens here, synchronously, so that an address already in use is
// reported as a startup error: silently counting on without the metrics somebody
// asked for in the config would show the monitoring side silence and let it
// conclude that everything is fine.
func (d *Daemon) startMetrics() (stop func(), err error) {
	if !d.cfg.Metrics.Enabled {
		d.log.Debug("metrics exporter disabled")
		return func() {}, nil
	}

	collector := metrics.NewCollector(d.cfg, d.cnt, d.store, metrics.Options{
		Start:   d.started,
		Version: d.version,
		Log:     d.log,
	})
	srv, err := metrics.NewServer(d.cfg.Metrics, collector, d.log)
	if err != nil {
		return nil, err
	}

	addr := srv.Addr()
	d.metricsAddr.Store(&addr)

	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := srv.Serve(); err != nil {
			d.log.Error("metrics exporter stopped", "error", err)
		}
	}()

	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), opTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			d.log.Warn("metrics exporter shutdown failed", "error", err)
		}
		<-served
		d.metricsAddr.Store(nil)
		d.log.Debug("metrics exporter stopped", "addr", addr)
	}, nil
}

// consume feeds lines from the tailer into the counter. It returns when the
// tailer is stopped and its channel closed.
func (d *Daemon) consume(t *tail.Tail) {
	for line := range t.Lines {
		if line.Err != nil {
			d.log.Warn("tail error", "error", line.Err)
			continue
		}
		if matched := d.cnt.ProcessLine(line.Text); len(matched) > 0 {
			// Per-line logging is noisy at high RPS, hence debug only.
			d.log.Debug("line matched", "actions", matched, "line", line.Text)
		}
	}
}

// Flush writes the buffered increments to Redis. Increments that cannot be
// written stay in the buffer and are retried on the next flush.
func (d *Daemon) Flush(ctx context.Context) {
	if !d.ensureInit(ctx) {
		// Nothing has been drained yet, so the buffer keeps growing untouched.
		d.log.Debug("flush deferred, redis is not reachable", "buffered", d.cnt.Pending())
		return
	}

	deltas := d.cnt.Drain()
	if len(deltas) == 0 && len(d.pendingHeartbeats) == 0 && len(d.pendingResets) == 0 {
		// Nothing to write, but the keys of an idle action still must not expire
		// while the daemon is running.
		d.refreshTTL(ctx)
		d.log.Debug("flush: nothing to do")
		return
	}

	now := time.Now()
	unwritten := make(map[string]int64)
	var written, errs int
	unreachable := false

	for _, action := range d.cfg.Actions {
		delta := deltas[action]

		// Once the connection is known to be down, the remaining actions would
		// only add one dial timeout each; they are retried on the next flush.
		if unreachable {
			if delta > 0 {
				unwritten[action] = delta
			}
			continue
		}

		ok, retryDelta, err := d.flushAction(ctx, action, delta, now)
		switch {
		case err != nil:
			errs++
			if retryDelta && delta > 0 {
				unwritten[action] = delta
			}
			d.noteRedisError("flush", err)
			if store.IsUnavailable(err) {
				unreachable = true
				d.log.Debug("flush stopped early, redis is unreachable", "action", action)
			}
		case ok:
			written++
		}
	}

	if len(unwritten) > 0 {
		d.cnt.Restore(unwritten)
	}
	if !unreachable {
		d.refreshTTL(ctx)
	}
	if errs == 0 {
		d.noteRedisOK()
	}
	d.log.Debug("flush done", "actions_written", written, "errors", errs, "buffered", d.cnt.Pending())
}

// flushAction writes the pending state of a single action. It reports whether
// anything was written and, on failure, whether delta still has to be returned
// to the buffer: an increment that Redis already accepted must never be retried.
func (d *Daemon) flushAction(ctx context.Context, action string, delta int64, now time.Time) (written, retryDelta bool, err error) {
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()

	// A scheduled reset that could not be written earlier takes priority: the
	// counter must be back at zero before new increments land on it.
	if d.pendingResets[action] {
		if err := d.store.Reset(octx, action, now); err != nil {
			return false, true, err
		}
		delete(d.pendingResets, action)
		delete(d.pendingHeartbeats, action)
	}

	total, hasPending := d.pendingHeartbeats[action]
	if delta == 0 && !hasPending {
		return false, false, nil
	}

	if delta > 0 {
		n, err := d.store.Incr(octx, action, delta)
		if err != nil {
			return false, true, err
		}
		total = n
	}

	if err := d.store.SetHeartbeat(octx, action, total, now); err != nil {
		// The increment is already in Redis: remember the total and retry only
		// the heartbeat value.
		d.pendingHeartbeats[action] = total
		return false, false, err
	}
	delete(d.pendingHeartbeats, action)
	return true, false, nil
}

// Reset flushes what is buffered and then zeroes every counter, so that the
// lines of the finishing interval are accounted for before the zeroing.
func (d *Daemon) Reset(ctx context.Context) {
	d.Flush(ctx)

	now := time.Now()
	var errs int
	unreachable := false
	for _, action := range d.cfg.Actions {
		if unreachable {
			d.pendingResets[action] = true
			errs++
			continue
		}
		octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
		err := d.store.Reset(octx, action, now)
		cancel()
		if err != nil {
			d.pendingResets[action] = true
			d.noteRedisError("reset", err)
			errs++
			unreachable = store.IsUnavailable(err)
			continue
		}
		delete(d.pendingHeartbeats, action)
		delete(d.pendingResets, action)
	}
	if errs == 0 {
		d.noteRedisOK()
		d.log.Info("counters reset", "actions", len(d.cfg.Actions))
	} else {
		d.log.Error("counters reset incomplete, will retry on next flush", "failed", errs)
	}
}

// ensureInit performs the one-off SETNX bootstrap, retrying until Redis answers.
// It reports whether the keys are ready.
func (d *Daemon) ensureInit(ctx context.Context) bool {
	if d.initialized {
		return true
	}
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	if err := d.store.Init(octx, d.cfg.Actions, time.Now()); err != nil {
		d.noteRedisError("init keys", err)
		return false
	}
	d.initialized = true
	d.noteRedisOK()
	d.log.Info("redis keys initialised", "actions", len(d.cfg.Actions),
		"ttl", d.cfg.Redis.TTLDuration())

	// Once at startup, unconditionally: with a TTL of 0 this drops an expiry
	// left over from a previous configuration, so the option is reversible.
	if err := d.store.Touch(octx, d.cfg.Actions); err != nil {
		d.noteRedisError("refresh ttl", err)
	}
	return true
}

// refreshTTL re-applies the key expiry. Keys of a live daemon must never expire,
// so this runs on every flush; with expiry disabled there is nothing to refresh
// (the one-off PERSIST already happened at startup).
func (d *Daemon) refreshTTL(ctx context.Context) {
	if d.store.TTL() <= 0 || !d.initialized {
		return
	}
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	if err := d.store.Touch(octx, d.cfg.Actions); err != nil {
		d.noteRedisError("refresh ttl", err)
	}
}

// noteRedisError logs the first error of an outage at warn level and keeps the
// repetitions at debug level, so an hour-long outage does not flood the log.
func (d *Daemon) noteRedisError(op string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if d.redisDown {
		d.log.Debug("redis still unavailable", "op", op, "error", err)
		return
	}
	d.redisDown = true
	d.log.Warn("redis unavailable, buffering in memory", "op", op, "error", err)
}

func (d *Daemon) noteRedisOK() {
	if d.redisDown {
		d.redisDown = false
		d.log.Info("redis is back, buffered increments flushed")
	}
}

// tailLogger routes the tailer's own messages (rotation, waiting for the file
// to appear) into the daemon log. It implements the unexported logger interface
// of nxadm/tail; the Fatal/Panic variants only log, the daemon decides on its own
// whether an error is fatal.
type tailLogger struct{ log *slog.Logger }

func (t tailLogger) msg(level slog.Level, s string) {
	t.log.Log(context.Background(), level, s, "component", "tail")
}

func (t tailLogger) Fatal(v ...any) { t.msg(slog.LevelError, fmt.Sprint(v...)) }
func (t tailLogger) Fatalf(format string, v ...any) {
	t.msg(slog.LevelError, fmt.Sprintf(format, v...))
}
func (t tailLogger) Fatalln(v ...any) { t.msg(slog.LevelError, fmt.Sprintln(v...)) }
func (t tailLogger) Panic(v ...any)   { t.msg(slog.LevelError, fmt.Sprint(v...)) }
func (t tailLogger) Panicf(format string, v ...any) {
	t.msg(slog.LevelError, fmt.Sprintf(format, v...))
}
func (t tailLogger) Panicln(v ...any)               { t.msg(slog.LevelError, fmt.Sprintln(v...)) }
func (t tailLogger) Print(v ...any)                 { t.msg(slog.LevelInfo, fmt.Sprint(v...)) }
func (t tailLogger) Printf(format string, v ...any) { t.msg(slog.LevelInfo, fmt.Sprintf(format, v...)) }
func (t tailLogger) Println(v ...any)               { t.msg(slog.LevelInfo, fmt.Sprintln(v...)) }
