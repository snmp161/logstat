// Package logging provides the daemon's own logger: a human readable slog
// handler writing either to stderr (picked up by journald) or to a file that
// can be reopened on SIGHUP after an external logrotate run.
package logging

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/snmp161/logstat/internal/config"
)

// Logger is an slog.Logger bound to a reopenable output.
type Logger struct {
	*slog.Logger
	out *output
}

// New builds a logger from the logging section of the configuration.
func New(cfg config.Logging) (*Logger, error) {
	level, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	out := &output{}
	if cfg.Output == config.OutputFile {
		out.path = cfg.File
		if err := out.open(); err != nil {
			return nil, err
		}
	} else {
		out.w = os.Stderr
	}

	return &Logger{
		Logger: slog.New(&handler{out: out, level: level}),
		out:    out,
	}, nil
}

// NewWriter builds a logger writing to an arbitrary writer. Used by tests and
// by the CLI before the configuration has been read.
func NewWriter(w io.Writer, level slog.Level) *Logger {
	out := &output{w: w}
	return &Logger{Logger: slog.New(&handler{out: out, level: level}), out: out}
}

// Reopen re-opens the log file, if the logger writes to one. It is a no-op for
// stderr output. Called on SIGHUP so that logrotate (rename or copytruncate)
// does not leave the daemon writing into a deleted or truncated file.
func (l *Logger) Reopen() error { return l.out.reopen() }

// Close releases the log file, if any.
func (l *Logger) Close() error { return l.out.close() }

// StdLogger returns a *log.Logger that forwards every line to this logger at
// the given level with the given attributes. Used to capture the output of
// libraries that only speak the standard log package (nxadm/tail).
func (l *Logger) StdLogger(level slog.Level, args ...any) *log.Logger {
	return log.New(&slogWriter{logger: l.Logger, level: level, args: args}, "", 0)
}

// ParseLevel maps a configuration level name to an slog.Level.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging.level must be one of %s, got %q",
			strings.Join(config.Levels, "/"), name)
	}
}

// output is a writer whose underlying file can be swapped under a mutex.
type output struct {
	mu   sync.Mutex
	w    io.Writer
	f    *os.File
	path string
}

func (o *output) open() error {
	f, err := os.OpenFile(o.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // operator-provided path
	if err != nil {
		return fmt.Errorf("open log file %s: %w", o.path, err)
	}
	o.f, o.w = f, f
	return nil
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.w.Write(p)
}

func (o *output) reopen() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.path == "" {
		return nil
	}
	old := o.f
	if err := o.open(); err != nil {
		return err
	}
	if old != nil {
		return old.Close()
	}
	return nil
}

func (o *output) close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f == nil {
		return nil
	}
	err := o.f.Close()
	o.f, o.w = nil, io.Discard
	return err
}

// handler renders records as "<rfc3339> <LEVEL> <message> key=value ...".
type handler struct {
	out    *output
	level  slog.Level
	attrs  []slog.Attr
	groups []string
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	nh.groups = append(append([]string{}, h.groups...), name)
	return &nh
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	b.WriteString(ts.Format(time.RFC3339))
	b.WriteByte(' ')
	fmt.Fprintf(&b, "%-5s", levelName(r.Level))
	b.WriteByte(' ')
	b.WriteString(r.Message)

	for _, a := range h.attrs {
		appendAttr(&b, h.groups, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.groups, a)
		return true
	})
	b.WriteByte('\n')

	_, err := h.out.Write([]byte(b.String()))
	return err
}

func appendAttr(b *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return
		}
		sub := groups
		if a.Key != "" {
			sub = append(append([]string{}, groups...), a.Key)
		}
		for _, sa := range attrs {
			appendAttr(b, sub, sa)
		}
		return
	}
	b.WriteByte(' ')
	for _, g := range groups {
		b.WriteString(g)
		b.WriteByte('.')
	}
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(quote(a.Value.String()))
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\"=") {
		return strconv.Quote(s)
	}
	return s
}

func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}

// slogWriter adapts the standard log package to slog.
type slogWriter struct {
	logger *slog.Logger
	level  slog.Level
	args   []any
}

func (w *slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		w.logger.Log(context.Background(), w.level, msg, w.args...)
	}
	return len(p), nil
}
