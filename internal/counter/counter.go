// Package counter implements the matching rule and the in-memory buffer of
// pending increments.
package counter

import (
	"strings"
	"sync"
)

// Counter accumulates per-action increments until they are drained into Redis.
//
// Matching rule (see the specification, §3): an action is counted once per line
// in which it occurs as a substring, no matter how many times it occurs in that
// line. Actions are matched independently of each other. The comparison honours
// the case unless the counter was built case-insensitive, in which case both the
// line and the actions are lowercased before the search.
type Counter struct {
	actions       []string
	caseSensitive bool
	// needles holds what is actually searched for, parallel to actions: the
	// actions themselves when the case counts, their lowercased form otherwise.
	needles []string

	mu      sync.Mutex
	pending map[string]int64
	// matched is the monotonic total per action since the start. It is kept
	// apart from pending because a flush empties the buffer, and a Prometheus
	// counter that fell back to zero on every flush would be worthless.
	matched map[string]int64
	lines   int64
}

// New returns a counter for the given actions. With caseSensitive false the
// matching ignores the case; the increments are still keyed by the action as it
// is spelled in the configuration, never as it appeared in the line.
func New(actions []string, caseSensitive bool) *Counter {
	c := &Counter{
		actions:       append([]string{}, actions...),
		caseSensitive: caseSensitive,
		needles:       make([]string, len(actions)),
		pending:       make(map[string]int64, len(actions)),
		matched:       make(map[string]int64, len(actions)),
	}
	for i, a := range c.actions {
		if caseSensitive {
			c.needles[i] = a
		} else {
			c.needles[i] = strings.ToLower(a)
		}
		// Every configured action is reported from the start, at zero, so that
		// "configured but never seen" does not look like "not configured".
		c.matched[a] = 0
	}
	return c
}

// Actions returns the configured actions, in configuration order.
func (c *Counter) Actions() []string { return append([]string{}, c.actions...) }

// CaseSensitive reports whether the matching honours the case.
func (c *Counter) CaseSensitive() bool { return c.caseSensitive }

// Match reports which actions occur in line, in configuration order.
func (c *Counter) Match(line string) []string {
	// One allocation per line instead of one per action: the needles are already
	// folded, so the line is the only thing left to lower.
	if !c.caseSensitive {
		line = strings.ToLower(line)
	}
	var matched []string
	for i, needle := range c.needles {
		if strings.Contains(line, needle) {
			matched = append(matched, c.actions[i])
		}
	}
	return matched
}

// ProcessLine applies the matching rule to a single log line and returns the
// actions that were incremented.
func (c *Counter) ProcessLine(line string) []string {
	matched := c.Match(line)
	c.mu.Lock()
	c.lines++
	for _, a := range matched {
		c.pending[a]++
		c.matched[a]++
	}
	c.mu.Unlock()
	return matched
}

// Drain removes and returns the buffered increments. Actions with a zero
// increment are not included.
func (c *Counter) Drain() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	out := c.pending
	c.pending = make(map[string]int64, len(c.actions))
	return out
}

// Restore puts increments back into the buffer, e.g. after a failed flush.
// It adds to whatever accumulated in the meantime instead of overwriting it.
func (c *Counter) Restore(deltas map[string]int64) {
	if len(deltas) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for a, n := range deltas {
		c.pending[a] += n
	}
}

// Matched returns a snapshot of the per-action totals since the start. Every
// configured action is present, unmatched ones with a zero. Restoring a failed
// flush does not count its increments a second time: the totals are advanced by
// ProcessLine only.
func (c *Counter) Matched() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.matched))
	for a, n := range c.matched {
		out[a] = n
	}
	return out
}

// PendingByAction returns a snapshot of what is buffered, per action. Every
// configured action is present, so a quiet action reads as a zero rather than
// as a gap.
func (c *Counter) PendingByAction() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.actions))
	for _, a := range c.actions {
		out[a] = c.pending[a]
	}
	return out
}

// Pending returns the total number of buffered increments across all actions.
func (c *Counter) Pending() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	for _, n := range c.pending {
		total += n
	}
	return total
}

// Lines returns how many log lines have been processed since start.
func (c *Counter) Lines() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lines
}
