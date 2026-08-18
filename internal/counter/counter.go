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
// in which it occurs as a case-sensitive substring, no matter how many times it
// occurs in that line. Actions are matched independently of each other.
type Counter struct {
	actions []string

	mu      sync.Mutex
	pending map[string]int64
	lines   int64
}

// New returns a counter for the given actions.
func New(actions []string) *Counter {
	return &Counter{
		actions: append([]string{}, actions...),
		pending: make(map[string]int64, len(actions)),
	}
}

// Actions returns the configured actions, in configuration order.
func (c *Counter) Actions() []string { return append([]string{}, c.actions...) }

// Match reports which actions occur in line, in configuration order.
func (c *Counter) Match(line string) []string {
	var matched []string
	for _, a := range c.actions {
		if strings.Contains(line, a) {
			matched = append(matched, a)
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
