package metrics

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/snmp161/logstat/internal/config"
	"github.com/snmp161/logstat/internal/counter"
)

func TestFormatUptime(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m 30s"},
		{59*time.Minute + 59*time.Second, "59m 59s"},
		{time.Hour, "1h 00m"},
		{time.Hour + 2*time.Minute + 30*time.Second, "1h 02m"},
		{25 * time.Hour, "1d 01h"},
		{50*24*time.Hour + 3*time.Hour, "50d 03h"},
	}
	for _, tc := range tests {
		if got := formatUptime(tc.d); got != tc.want {
			t.Errorf("formatUptime(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// The page a human opens in a browser: the same numbers as the metrics, laid
// out to be read rather than parsed.
func TestStatusPageOnTheRoot(t *testing.T) {
	cfg := testConfig()
	cnt := counter.New(cfg.Actions, true)
	cnt.ProcessLine("get-number") // one match in memory, still buffered

	// get-sms exists in Redis, get-number does not: a missing key must read as
	// a dash, not as a zero.
	rd := &stubReader{counters: map[string]int64{"get-sms": 903}}
	_, base := newTestServerWith(t, cfg, cnt, rd)

	resp, body := getFull(t, base+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	for _, want := range []string{
		"logstat",     // the name and version in the header
		"v9.9.9-test", // the version the daemon passed in
		testHost,      // the instance the Redis keys belong to
		"1m 30s",      // uptime, from the fake clock
		"get-number",  // every configured word is listed
		"get-sms",
		"903",            // the value read from Redis
		cfg.Metrics.Path, // the link to the exposition endpoint
		"10.0.0.5:6380",  // the Redis this instance talks to
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the status page does not contain %q\nbody:\n%s", want, body)
		}
	}
	if strings.Contains(body, cfg.Redis.Password) {
		t.Fatal("the status page shows the Redis password")
	}
	// A word whose key is missing from Redis shows a dash instead of a zero.
	if !strings.Contains(body, "—") {
		t.Errorf("a missing Redis key must render as a dash\nbody:\n%s", body)
	}
}

// Whatever a config value contains, it lands on the page as text: the config is
// operator input, and the page is HTML.
func TestStatusPageEscapesConfigValues(t *testing.T) {
	cfg := testConfig()
	cfg.LogPath = `/var/log/<script>alert("xss")</script>.log`
	_, base := newTestServerWith(t, cfg, counter.New(cfg.Actions, true), &stubReader{})

	_, body := getFull(t, base+"/")
	if strings.Contains(body, "<script>alert") {
		t.Fatalf("the log path is not escaped\nbody:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the escaped log path is missing from the page\nbody:\n%s", body)
	}
}

// An unreachable Redis must not take the page down: what lives in this process
// is exactly what an operator wants to see during an outage.
func TestStatusPageWhileRedisIsDown(t *testing.T) {
	cfg := testConfig()
	cnt := counter.New(cfg.Actions, true)
	cnt.ProcessLine("get-number")
	rd := &stubReader{err: context.DeadlineExceeded}

	_, base := newTestServerWith(t, cfg, cnt, rd)

	resp, body := getFull(t, base+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / with Redis down = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "unreachable") {
		t.Errorf("the page must say that Redis is unreachable\nbody:\n%s", body)
	}
	if !strings.Contains(body, "get-number") {
		t.Error("the in-memory numbers must still be listed during an outage")
	}
}

// With the exposition mounted on the root there is nowhere left for the status
// page, and the exposition wins: that is the documented trade-off.
func TestStatusPageAbsentWhenMetricsAreOnTheRoot(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.Path = "/"
	mcfg := config.Metrics{Enabled: true, Listen: "127.0.0.1:0", Path: "/"}
	srv := startServer(t, mcfg, cfg, counter.New(cfg.Actions, true), &stubReader{})

	resp, body := getFull(t, "http://"+srv.Addr()+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "logstat_uptime_seconds") {
		t.Errorf("the root must serve the exposition, got:\n%s", body)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want the exposition format", ct)
	}
}
