// Package store implements the Redis key schema of logstat: an integer counter
// per action and an optional preformatted heartbeat value for the external
// monitoring reader.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
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

// HeartbeatKey returns the key of the preformatted monitoring value for
// host/action. It is only written when the heartbeat_key option is on.
func HeartbeatKey(host, action string) string {
	return KeyPrefix + "heartbeat:" + host + ":" + action
}

// FormatHeartbeat renders the monitoring value. The timestamp is ISO-8601 with a
// timezone offset, i.e. the format produced by `date -Iseconds`.
func FormatHeartbeat(host string, ts time.Time, action string, lines int64) string {
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
	// A value we could not parse came back over a working connection, so it is
	// the opposite of an outage however unlike a server reply it looks.
	if errors.Is(err, ErrMalformedValue) {
		return false
	}
	// Every error the server itself replied with implements redis.Error, so the
	// connection was fine; anything else is a transport level problem.
	var replyErr redis.Error
	return !errors.As(err, &replyErr)
}

// Store writes the counters of one host to one Redis instance.
type Store struct {
	rdb       *redis.Client
	host      string
	heartbeat bool
	ttl       time.Duration
}

// New connects (lazily, go-redis dials on first use) to the configured Redis.
// heartbeat mirrors the heartbeat_key option: when false, only the integer
// counters are maintained and the monitoring value is never written.
func New(cfg config.Redis, host string, heartbeat bool) *Store {
	return NewWithClient(redis.NewClient(&redis.Options{
		Addr:         cfg.Addr(),
		DB:           cfg.DB,
		Password:     cfg.Password,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}), host, heartbeat, cfg.TTLDuration())
}

// NewWithClient wraps an existing client. Used by tests against miniredis.
func NewWithClient(rdb *redis.Client, host string, heartbeat bool, ttl time.Duration) *Store {
	return &Store{rdb: rdb, host: host, heartbeat: heartbeat, ttl: ttl}
}

// Host returns the host name embedded in the keys.
func (s *Store) Host() string { return s.host }

// HeartbeatEnabled reports whether the monitoring value is maintained.
func (s *Store) HeartbeatEnabled() bool { return s.heartbeat }

// TTL returns the expiry applied to the keys, 0 meaning no expiry.
func (s *Store) TTL() time.Duration { return s.ttl }

// keys returns every key logstat maintains for action.
func (s *Store) keys(action string) []string {
	if !s.heartbeat {
		return []string{CounterKey(s.host, action)}
	}
	return []string{CounterKey(s.host, action), HeartbeatKey(s.host, action)}
}

// Close releases the Redis connection pool.
func (s *Store) Close() error { return s.rdb.Close() }

// Init creates the keys of actions that do not exist yet without touching
// existing values, so that a restart never loses what was already counted and a
// fresh install is immediately readable by the monitoring side.
func (s *Store) Init(ctx context.Context, actions []string, ts time.Time) error {
	for _, a := range actions {
		if err := s.rdb.SetNX(ctx, CounterKey(s.host, a), 0, s.ttl).Err(); err != nil {
			return fmt.Errorf("init counter for %q: %w", a, err)
		}
		if !s.heartbeat {
			continue
		}
		n, err := s.rdb.Get(ctx, CounterKey(s.host, a)).Int64()
		if err != nil {
			return fmt.Errorf("init counter for %q: %w", a, err)
		}
		if err := s.rdb.SetNX(ctx, HeartbeatKey(s.host, a), FormatHeartbeat(s.host, ts, a, n), s.ttl).Err(); err != nil {
			return fmt.Errorf("init heartbeat for %q: %w", a, err)
		}
	}
	return nil
}

// Touch re-applies the configured expiry to every key of every action, so that
// the keys of a live daemon never expire and the TTL only measures how long they
// outlive it. With a TTL of 0 it removes an expiry left over from an earlier
// configuration, which makes the option reversible without manual cleanup.
func (s *Store) Touch(ctx context.Context, actions []string) error {
	if len(actions) == 0 {
		return nil
	}
	pipe := s.rdb.Pipeline()
	for _, a := range actions {
		for _, k := range s.keys(a) {
			if s.ttl > 0 {
				pipe.Expire(ctx, k, s.ttl)
			} else {
				pipe.Persist(ctx, k)
			}
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("refresh ttl: %w", err)
	}
	return nil
}

// Incr adds delta to the integer counter and returns the new total. INCRBY
// leaves the expiry of an existing key untouched, so the TTL is re-applied in
// the same round trip.
func (s *Store) Incr(ctx context.Context, action string, delta int64) (int64, error) {
	key := CounterKey(s.host, action)
	if s.ttl <= 0 {
		n, err := s.rdb.IncrBy(ctx, key, delta).Result()
		if err != nil {
			return 0, fmt.Errorf("incrby %q: %w", action, err)
		}
		return n, nil
	}

	pipe := s.rdb.Pipeline()
	incr := pipe.IncrBy(ctx, key, delta)
	expire := pipe.Expire(ctx, key, s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("incrby %q: %w", action, err)
	}
	if err := incr.Err(); err != nil {
		return 0, fmt.Errorf("incrby %q: %w", action, err)
	}
	if err := expire.Err(); err != nil {
		return 0, fmt.Errorf("expire %q: %w", action, err)
	}
	return incr.Val(), nil
}

// SetHeartbeat writes the preformatted monitoring value for action. It is a
// no-op when the heartbeat key is disabled, so callers do not have to branch.
func (s *Store) SetHeartbeat(ctx context.Context, action string, lines int64, ts time.Time) error {
	if !s.heartbeat {
		return nil
	}
	if err := s.rdb.Set(ctx, HeartbeatKey(s.host, action), FormatHeartbeat(s.host, ts, action, lines), s.ttl).Err(); err != nil {
		return fmt.Errorf("set heartbeat %q: %w", action, err)
	}
	return nil
}

// Reset zeroes the counter of action and writes the matching lines=0 heartbeat.
func (s *Store) Reset(ctx context.Context, action string, ts time.Time) error {
	if err := s.rdb.Set(ctx, CounterKey(s.host, action), 0, s.ttl).Err(); err != nil {
		return fmt.Errorf("reset counter %q: %w", action, err)
	}
	return s.SetHeartbeat(ctx, action, 0, ts)
}

// ErrMalformedValue reports a counter key holding something that is not an
// integer, which means somebody else is writing into it. It is deliberately
// distinct from an unreachable Redis: only one of the two means the instance
// lost its connection, and the metrics exporter has to tell them apart.
var ErrMalformedValue = errors.New("counter value is not an integer")

// Counters returns the value of the integer counter of every action in one
// round trip. An action whose key does not exist (never created, or expired) is
// absent from the result rather than reported as a zero: the metrics exporter
// must not invent a value that cannot be told from a counter really at zero.
//
// A key that cannot be parsed is skipped the same way and reported through an
// ErrMalformedValue error, alongside the actions that did parse: one foreign
// key must not black out the counters of every other word.
func (s *Store) Counters(ctx context.Context, actions []string) (map[string]int64, error) {
	if len(actions) == 0 {
		return map[string]int64{}, nil
	}
	keys := make([]string, len(actions))
	for i, a := range actions {
		keys[i] = CounterKey(s.host, a)
	}
	values, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read counters: %w", err)
	}
	// MGET answers one value per key. Checking that instead of trusting it keeps
	// a protocol surprise (a proxy in front of Redis, say) an error rather than
	// a panic in the middle of a scrape.
	if len(values) != len(actions) {
		return nil, fmt.Errorf("read counters: got %d values for %d keys", len(values), len(actions))
	}

	out := make(map[string]int64, len(actions))
	var malformed []string
	for i := range actions {
		v := values[i]
		if v == nil {
			continue
		}
		raw, ok := v.(string)
		if !ok {
			malformed = append(malformed, actions[i])
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			malformed = append(malformed, actions[i])
			continue
		}
		out[actions[i]] = n
	}
	if len(malformed) > 0 {
		return out, fmt.Errorf("read counters of %s: %w", strings.Join(malformed, ", "), ErrMalformedValue)
	}
	return out, nil
}
