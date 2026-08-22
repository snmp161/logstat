package counter

import (
	"reflect"
	"sync"
	"testing"
)

var defaultActions = []string{"get-number", "get-sms", "getNumber", "getStatus"}

func TestProcessLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{
			name: "substring anywhere in the line",
			line: `10.0.0.1 - - [18/Aug/2026:15:04:05 +0300] "GET /api/get-number?x=1 HTTP/1.1" 200`,
			want: []string{"get-number"},
		},
		{
			name: "at the very beginning",
			line: "get-sms and nothing else",
			want: []string{"get-sms"},
		},
		{
			name: "at the very end",
			line: "trailing getStatus",
			want: []string{"getStatus"},
		},
		{
			name: "counted once per line even with two occurrences",
			line: "get-number then get-number again",
			want: []string{"get-number"},
		},
		{
			name: "several different words in one line",
			line: "getNumber and get-sms in the same line",
			want: []string{"get-sms", "getNumber"},
		},
		{
			name: "case sensitive: GET-NUMBER does not match",
			line: "GET-NUMBER GETNUMBER Getstatus",
			want: nil,
		},
		{
			name: "case sensitive: getnumber does not match getNumber",
			line: "getnumber",
			want: nil,
		},
		{
			name: "no match",
			line: `"GET /health HTTP/1.1" 200`,
			want: nil,
		},
		{
			name: "empty line",
			line: "",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := New(defaultActions, true)
			got := c.ProcessLine(tc.line)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ProcessLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
			for _, a := range tc.want {
				if c.Drain0(a) != 1 {
					t.Fatalf("counter of %q must be 1", a)
				}
			}
		})
	}
}

// Drain0 is a small test helper: it peeks at the pending value of one action.
func (c *Counter) Drain0(action string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending[action]
}

func TestMatchIsIndependentOfOrder(t *testing.T) {
	c := New([]string{"getStatus", "get-sms"}, true)
	got := c.match("get-sms getStatus")
	want := []string{"getStatus", "get-sms"} // configuration order, not line order
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Match = %v, want %v", got, want)
	}
}

// A word that is a substring of another word makes both counters advance. The
// configuration validator warns about this; the matcher itself does not special
// case it, and this test pins that behaviour down.
func TestPrefixOverlapIncrementsBoth(t *testing.T) {
	c := New([]string{"getStatus", "getStatusExtended"}, true)
	got := c.ProcessLine("call getStatusExtended once")
	want := []string{"getStatus", "getStatusExtended"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProcessLine = %v, want %v", got, want)
	}
}

// With case_sensitive: no the line and the code words are folded before the
// substring search, so any spelling in the log feeds the counter of the word.
func TestProcessLineCaseInsensitive(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{
			name: "the configured spelling still matches",
			line: `"GET /api/get-number?x=1 HTTP/1.1" 200`,
			want: []string{"get-number"},
		},
		{
			name: "upper case in the line",
			line: "GET-NUMBER",
			want: []string{"get-number"},
		},
		{
			name: "mixed case in the line",
			line: "call Get-Number now",
			want: []string{"get-number"},
		},
		{
			name: "a word spelled in the config with capitals matches lower case",
			line: "getnumber and getstatus",
			want: []string{"getNumber", "getStatus"},
		},
		{
			name: "still counted once per line",
			line: "GET-NUMBER then get-number again",
			want: []string{"get-number"},
		},
		{
			name: "several words in one line, configuration order",
			line: "GETSTATUS after GET-SMS",
			want: []string{"get-sms", "getStatus"},
		},
		{
			name: "a word that is simply absent still does not match",
			line: `"GET /health HTTP/1.1" 200`,
			want: nil,
		},
		{
			name: "empty line",
			line: "",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := New(defaultActions, false)
			got := c.ProcessLine(tc.line)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ProcessLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
			for _, a := range tc.want {
				if c.Drain0(a) != 1 {
					t.Fatalf("counter of %q must be 1", a)
				}
			}
		})
	}
}

// The case of the log line must not leak into the counter keys: whatever the
// line looked like, the increment belongs to the word as it is spelled in the
// configuration, which is what the Redis key is built from.
func TestCaseInsensitiveKeepsTheConfiguredSpelling(t *testing.T) {
	c := New([]string{"getNumber"}, false)
	if got := c.ProcessLine("GETNUMBER"); !reflect.DeepEqual(got, []string{"getNumber"}) {
		t.Fatalf("ProcessLine = %v, want [getNumber]", got)
	}
	c.ProcessLine("getnumber")

	want := map[string]int64{"getNumber": 2}
	if got := c.Drain(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Drain = %v, want %v", got, want)
	}
	if _, ok := c.Matched()["getNumber"]; !ok {
		t.Fatalf("Matched = %v, folding must not rewrite the configured words", c.Matched())
	}
}

// Folding is not limited to ASCII: strings.ToLower covers the same alphabets a
// log line may carry.
func TestCaseInsensitiveFoldsNonASCII(t *testing.T) {
	c := New([]string{"Получить-Номер"}, false)
	if got := c.ProcessLine("метод ПОЛУЧИТЬ-НОМЕР вызван"); !reflect.DeepEqual(got, []string{"Получить-Номер"}) {
		t.Fatalf("ProcessLine = %v, want [Получить-Номер]", got)
	}
}

// Overlaps that only exist without case behave like any other overlap: both
// counters advance. The config validator is the place that warns about them.
func TestCaseInsensitiveOverlapIncrementsBoth(t *testing.T) {
	c := New([]string{"getstatus", "GetStatusExtended"}, false)
	got := c.ProcessLine("call GETSTATUSEXTENDED once")
	want := []string{"getstatus", "GetStatusExtended"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProcessLine = %v, want %v", got, want)
	}
}

// Two words differing only in case keep separate counters; the validator warns
// about the pair, the matcher just increments both.
func TestCaseInsensitiveTwinsKeepSeparateCounters(t *testing.T) {
	c := New([]string{"getNumber", "GETNUMBER"}, false)
	c.ProcessLine("getnumber")

	want := map[string]int64{"getNumber": 1, "GETNUMBER": 1}
	if got := c.Drain(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Drain = %v, want %v", got, want)
	}
}

func TestAccumulateAndDrain(t *testing.T) {
	c := New(defaultActions, true)
	c.ProcessLine("get-number")
	c.ProcessLine("get-number and get-sms")
	c.ProcessLine("nothing here")

	if got := c.Lines(); got != 3 {
		t.Errorf("Lines = %d, want 3", got)
	}
	if got := c.Pending(); got != 3 {
		t.Errorf("Pending = %d, want 3", got)
	}

	got := c.Drain()
	want := map[string]int64{"get-number": 2, "get-sms": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Drain = %v, want %v", got, want)
	}
	if c.Drain() != nil {
		t.Fatal("second Drain must be empty")
	}
	if got := c.Pending(); got != 0 {
		t.Errorf("Pending after Drain = %d, want 0", got)
	}
}

// The exporter needs a per-action view of both halves of the counter: the total
// since the start, which only grows, and what is still waiting for a flush.
func TestMatchedAndPendingByAction(t *testing.T) {
	c := New(defaultActions, true)

	// Every configured action is present from the start, at zero: "configured
	// but never seen" must not look like "not configured" in the metrics.
	zero := map[string]int64{"get-number": 0, "get-sms": 0, "getNumber": 0, "getStatus": 0}
	if got := c.Matched(); !reflect.DeepEqual(got, zero) {
		t.Fatalf("Matched of a fresh counter = %v, want %v", got, zero)
	}
	if got := c.PendingByAction(); !reflect.DeepEqual(got, zero) {
		t.Fatalf("PendingByAction of a fresh counter = %v, want %v", got, zero)
	}

	c.ProcessLine("get-number")
	c.ProcessLine("get-number and get-sms")
	c.ProcessLine("nothing here")

	want := map[string]int64{"get-number": 2, "get-sms": 1, "getNumber": 0, "getStatus": 0}
	if got := c.Matched(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Matched = %v, want %v", got, want)
	}
	if got := c.PendingByAction(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PendingByAction = %v, want %v", got, want)
	}

	// A flush empties the buffer but must not touch the totals: a Prometheus
	// counter that fell back to zero every ten seconds would be useless.
	c.Drain()
	if got := c.Matched(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Matched after Drain = %v, want %v", got, want)
	}
	if got := c.PendingByAction(); !reflect.DeepEqual(got, zero) {
		t.Fatalf("PendingByAction after Drain = %v, want %v", got, zero)
	}

	// A failed flush puts the increments back into the buffer; the totals must
	// not count them a second time.
	c.Restore(map[string]int64{"get-number": 2})
	if got := c.Matched()["get-number"]; got != 2 {
		t.Fatalf("Matched[get-number] after Restore = %d, want 2", got)
	}
	if got := c.PendingByAction()["get-number"]; got != 2 {
		t.Fatalf("PendingByAction[get-number] after Restore = %d, want 2", got)
	}
}

// The maps are snapshots: a caller (the metrics collector) may keep or mutate
// them without disturbing the counter.
func TestMatchedAndPendingAreSnapshots(t *testing.T) {
	c := New([]string{"a"}, true)
	c.ProcessLine("a")

	matched := c.Matched()
	matched["a"] = 99
	pending := c.PendingByAction()
	pending["a"] = 99

	if got := c.Matched()["a"]; got != 1 {
		t.Fatalf("Matched[a] = %d, want 1", got)
	}
	if got := c.PendingByAction()["a"]; got != 1 {
		t.Fatalf("PendingByAction[a] = %d, want 1", got)
	}
}

func TestRestoreAddsToWhatArrivedMeanwhile(t *testing.T) {
	c := New(defaultActions, true)
	c.ProcessLine("get-number")
	drained := c.Drain()

	// A line arrives while the flush is in flight, then the flush fails.
	c.ProcessLine("get-number")
	c.Restore(drained)

	if got := c.Drain()["get-number"]; got != 2 {
		t.Fatalf("get-number = %d, want 2", got)
	}
	c.Restore(nil) // must not panic
}

func TestNewCopiesItsInput(t *testing.T) {
	src := []string{"a", "b"}
	c := New(src, true)
	src[0] = "mutated"

	c.ProcessLine("a and b")
	got := c.Matched()
	if got["a"] != 1 || got["b"] != 1 {
		t.Fatalf("Matched = %v, the counter must not alias the slice it was given", got)
	}
}

func TestConcurrentProcessAndDrain(t *testing.T) {
	c := New(defaultActions, true)
	const writers, perWriter = 8, 500

	total := map[string]int64{}
	stop := make(chan struct{})
	drainerDone := make(chan struct{})

	// One drainer competing with the writers, exactly like the flush loop
	// competes with the tail goroutine in the daemon.
	go func() {
		defer close(drainerDone)
		for {
			for a, n := range c.Drain() {
				total[a] += n
			}
			select {
			case <-stop:
				for a, n := range c.Drain() {
					total[a] += n
				}
				return
			default:
			}
		}
	}()

	// A scraper reading the exporter's view while the writers and the drainer
	// are busy: the metrics endpoint runs in its own goroutine in the daemon.
	scraperDone := make(chan struct{})
	go func() {
		defer close(scraperDone)
		for {
			c.Matched()
			c.PendingByAction()
			c.Lines()
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	var writersWG sync.WaitGroup
	for i := 0; i < writers; i++ {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			for j := 0; j < perWriter; j++ {
				c.ProcessLine("x get-number y get-number z get-sms")
			}
		}()
	}
	writersWG.Wait()
	close(stop)
	<-drainerDone
	<-scraperDone

	if total["get-number"] != writers*perWriter {
		t.Errorf("get-number = %d, want %d", total["get-number"], writers*perWriter)
	}
	if total["get-sms"] != writers*perWriter {
		t.Errorf("get-sms = %d, want %d", total["get-sms"], writers*perWriter)
	}
	// Whatever the drainer collected, the totals must agree with it.
	matched := c.Matched()
	if matched["get-number"] != total["get-number"] || matched["get-sms"] != total["get-sms"] {
		t.Errorf("Matched = %v, want it to agree with the drained totals %v", matched, total)
	}
}
