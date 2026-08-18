// Package store implements the Redis key schema of logstat: an integer counter
// per action and a preformatted value for the external monitoring reader.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/snmp161/logstat/internal/config"
)

// KeyPrefix is the common prefix of every key written by logstat.
const KeyPrefix = "logstat:"

// CounterKey returns the key of the integer counter for host/action.
func CounterKey(host, action string) string {
	return KeyPrefix + "counter:" + host + ":" + action
}

// ValueKey returns the key of the preformatted monitoring value for host/action.
func ValueKey(host, action string) string {
	return KeyPrefix + host + "_type=" + action
}

// FormatValue renders the monitoring value. The timestamp is ISO-8601 with a
// timezone offset, i.e. the format produced by `date -Iseconds`.
func FormatValue(host string, ts time.Time, action string, lines int64) string {
	return fmt.Sprintf("server=%s time=%s type=%s lines=%d",
		host, ts.Format(time.RFC3339), action, lines)
}

// ShortHostname returns the host name up to the first dot, the equivalent of
// `hostname -s`. It falls back to "unknown" if the host name is unavailable.
func ShortHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// SetLibraryLogger routes the internal messages of go-redis (pool dial
// failures and the like) into lg at debug level, so that a Redis outage is
// reported once by the daemon instead of on every reconnect attempt.
func SetLibraryLogger(lg *slog.Logger) {
	redis.SetLogger(libraryLogger{lg: lg})
}

type libraryLogger struct{ lg *slog.Logger }

func (l libraryLogger) Printf(ctx context.Context, format string, v ...any) {
	l.lg.Log(ctx, slog.LevelDebug, strings.TrimRight(fmt.Sprintf(format, v...), "\n"), "component", "go-redis")
}

// IsUnavailable reports whether err means the Redis instance could not be
// reached at all (dial failure, timeout, broken connection) as opposed to a
// server that answered with an error. During an outage the daemon uses this to
// stop retrying the remaining actions of the current flush instead of waiting
// for one dial timeout per action.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// Every error the server itself replied with implements redis.Error, so the
	// connection was fine; anything else is a transport level problem.
	var replyErr redis.Error
	return !errors.As(err, &replyErr)
}

// Store writes the counters of one host to one Redis instance.
type Store struct {
	rdb  *redis.Client
	host string
}

// New connects (lazily, go-redis dials on first use) to the configured Redis.
func New(cfg config.Redis, host string) *Store {
	return NewWithClient(redis.NewClient(&redis.Options{
		Addr:         cfg.Addr(),
		DB:           cfg.DB,
		Password:     cfg.Password,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}), host)
}

// NewWithClient wraps an existing client. Used by tests against miniredis.
func NewWithClient(rdb *redis.Client, host string) *Store {
	return &Store{rdb: rdb, host: host}
}

// Host returns the host name embedded in the keys.
func (s *Store) Host() string { return s.host }

// Close releases the Redis connection pool.
func (s *Store) Close() error { return s.rdb.Close() }

// Ping checks that Redis is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.rdb.Ping(ctx).Err() }

// Init creates the keys of actions that do not exist yet without touching
// existing values, so that a restart never loses what was already counted and a
// fresh install is immediately readable by the monitoring side.
func (s *Store) Init(ctx context.Context, actions []string, ts time.Time) error {
	for _, a := range actions {
		if err := s.rdb.SetNX(ctx, CounterKey(s.host, a), 0, 0).Err(); err != nil {
			return fmt.Errorf("init counter for %q: %w", a, err)
		}
		n, err := s.rdb.Get(ctx, CounterKey(s.host, a)).Int64()
		if err != nil {
			return fmt.Errorf("init counter for %q: %w", a, err)
		}
		if err := s.rdb.SetNX(ctx, ValueKey(s.host, a), FormatValue(s.host, ts, a, n), 0).Err(); err != nil {
			return fmt.Errorf("init value for %q: %w", a, err)
		}
	}
	return nil
}

// Incr adds delta to the integer counter and returns the new total.
func (s *Store) Incr(ctx context.Context, action string, delta int64) (int64, error) {
	n, err := s.rdb.IncrBy(ctx, CounterKey(s.host, action), delta).Result()
	if err != nil {
		return 0, fmt.Errorf("incrby %q: %w", action, err)
	}
	return n, nil
}

// SetValue writes the preformatted monitoring value for action.
func (s *Store) SetValue(ctx context.Context, action string, lines int64, ts time.Time) error {
	if err := s.rdb.Set(ctx, ValueKey(s.host, action), FormatValue(s.host, ts, action, lines), 0).Err(); err != nil {
		return fmt.Errorf("set value %q: %w", action, err)
	}
	return nil
}

// Reset zeroes the counter of action and writes the matching lines=0 value.
func (s *Store) Reset(ctx context.Context, action string, ts time.Time) error {
	if err := s.rdb.Set(ctx, CounterKey(s.host, action), 0, 0).Err(); err != nil {
		return fmt.Errorf("reset counter %q: %w", action, err)
	}
	return s.SetValue(ctx, action, 0, ts)
}

// Get returns the current value of the integer counter for action.
func (s *Store) Get(ctx context.Context, action string) (int64, error) {
	n, err := s.rdb.Get(ctx, CounterKey(s.host, action)).Int64()
	if err != nil {
		return 0, fmt.Errorf("get counter %q: %w", action, err)
	}
	return n, nil
}
