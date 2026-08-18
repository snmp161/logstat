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
			c := New(defaultActions)
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
	c := New([]string{"getStatus", "get-sms"})
	got := c.Match("get-sms getStatus")
	want := []string{"getStatus", "get-sms"} // configuration order, not line order
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Match = %v, want %v", got, want)
	}
}

// A word that is a substring of another word makes both counters advance. The
// configuration validator warns about this; the matcher itself does not special
// case it, and this test pins that behaviour down.
func TestPrefixOverlapIncrementsBoth(t *testing.T) {
	c := New([]string{"getStatus", "getStatusExtended"})
	got := c.ProcessLine("call getStatusExtended once")
	want := []string{"getStatus", "getStatusExtended"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProcessLine = %v, want %v", got, want)
	}
}

func TestAccumulateAndDrain(t *testing.T) {
	c := New(defaultActions)
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

func TestRestoreAddsToWhatArrivedMeanwhile(t *testing.T) {
	c := New(defaultActions)
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

func TestActionsIsACopy(t *testing.T) {
	src := []string{"a", "b"}
	c := New(src)
	src[0] = "mutated"
	if got := c.Actions(); got[0] != "a" {
		t.Fatalf("Actions = %v, counter must not alias its input", got)
	}
	got := c.Actions()
	got[1] = "mutated"
	if c.Actions()[1] != "b" {
		t.Fatal("Actions must return a copy")
	}
}

func TestConcurrentProcessAndDrain(t *testing.T) {
	c := New(defaultActions)
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

	if total["get-number"] != writers*perWriter {
		t.Errorf("get-number = %d, want %d", total["get-number"], writers*perWriter)
	}
	if total["get-sms"] != writers*perWriter {
		t.Errorf("get-sms = %d, want %d", total["get-sms"], writers*perWriter)
	}
}
