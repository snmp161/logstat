package metrics

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/snmp161/logstat/internal/config"
	"github.com/snmp161/logstat/internal/counter"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startServer runs an exporter for the duration of the test and stops it
// afterwards.
func startServer(t *testing.T, mcfg config.Metrics, cfg *config.Config, cnt *counter.Counter, rd Reader) *Server {
	t.Helper()

	srv, err := NewServer(mcfg, newTestCollector(t, cfg, cnt, rd), discardLogger())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return srv
}

// newTestServer starts an exporter on a port picked by the kernel and returns
// it together with its base URL.
func newTestServer(t *testing.T, path string, rd Reader, cfg *config.Config) (*Server, string) {
	t.Helper()
	mcfg := config.Metrics{Enabled: true, Listen: "127.0.0.1:0", Path: path}
	srv := startServer(t, mcfg, cfg, counter.New(cfg.Actions, true), rd)
	return srv, "http://" + srv.Addr()
}

// newTestServerWith is newTestServer for a test that cares about the counter it
// starts from, on the path the config carries.
func newTestServerWith(t *testing.T, cfg *config.Config, cnt *counter.Counter, rd Reader) (*Server, string) {
	t.Helper()
	mcfg := config.Metrics{Enabled: true, Listen: "127.0.0.1:0", Path: cfg.Metrics.Path}
	srv := startServer(t, mcfg, cfg, cnt, rd)
	return srv, "http://" + srv.Addr()
}

func getFull(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, body := getFull(t, url)
	return resp.StatusCode, body
}

func TestServerServesTheConfiguredPath(t *testing.T) {
	cfg := testConfig()
	rd := &stubReader{counters: map[string]int64{"get-number": 12}}
	_, base := newTestServer(t, "/metrics", rd, cfg)

	code, body := get(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", code)
	}
	for _, want := range []string{
		"logstat_uptime_seconds 90",
		`logstat_matched_lines_total{action="get-number"} 0`,
		`logstat_redis_counter{action="get-number"} 12`,
		"logstat_redis_up 1",
		"logstat_config_info{",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the response does not contain %q\nbody:\n%s", want, body)
		}
	}
	// The password of the config used by the tests must not be in the payload.
	if strings.Contains(body, cfg.Redis.Password) {
		t.Error("the exposition output contains the Redis password")
	}
}

func TestServerCustomPathAnd404(t *testing.T) {
	cfg := testConfig()
	_, base := newTestServer(t, "/internal/metrics", &stubReader{}, cfg)

	if code, _ := get(t, base+"/internal/metrics"); code != http.StatusOK {
		t.Errorf("GET /internal/metrics = %d, want 200", code)
	}
	// The root is the status page; everything else is a 404, so a scrape
	// pointed at the wrong path fails loudly instead of returning nothing.
	if code, _ := get(t, base+"/"); code != http.StatusOK {
		t.Errorf("GET / = %d, want 200 (the status page)", code)
	}
	for _, path := range []string{"/metrics", "/internal", "/internal/metrics/extra"} {
		if code, _ := get(t, base+path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}

// The scrape must survive an unreachable Redis: an exporter that fails the
// whole scrape would hide the metrics that have nothing to do with Redis.
func TestServerAnswersWhileRedisIsDown(t *testing.T) {
	cfg := testConfig()
	rd := &stubReader{err: context.DeadlineExceeded}
	_, base := newTestServer(t, "/metrics", rd, cfg)

	code, body := get(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics with Redis down = %d, want 200", code)
	}
	if !strings.Contains(body, "logstat_redis_up 0") {
		t.Errorf("want logstat_redis_up 0 in the body:\n%s", body)
	}
	if !strings.Contains(body, "logstat_uptime_seconds 90") {
		t.Error("the metrics that do not need Redis must still be exported")
	}
	if strings.Contains(body, "logstat_redis_counter{") {
		t.Error("no logstat_redis_counter series may be exported while Redis is unreachable")
	}
}

// A port that is already taken has to surface as an error from NewServer, so
// that the daemon can refuse to start instead of running without metrics.
func TestNewServerFailsOnATakenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	cfg := testConfig()
	mcfg := config.Metrics{Enabled: true, Listen: ln.Addr().String(), Path: "/metrics"}
	srv, err := NewServer(mcfg, newTestCollector(t, cfg, counter.New(cfg.Actions, true), &stubReader{}), discardLogger())
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		t.Fatal("NewServer must fail when the port is already in use")
	}
	if !strings.Contains(err.Error(), ln.Addr().String()) {
		t.Errorf("error = %v, want it to name the address", err)
	}
}

// A path that net/http would read as a routing pattern must come back as an
// error. The config validator rejects it too, but NewServer is reachable
// without that validation, and http.ServeMux answers a broken pattern with a
// panic — a config typo must not take the process down that way.
func TestNewServerRejectsAPathThatIsARoutingPattern(t *testing.T) {
	for _, path := range []string{"/met{rics", "/{env}", "/metrics extra", "/metrics}"} {
		t.Run(path, func(t *testing.T) {
			cfg := testConfig()
			mcfg := config.Metrics{Enabled: true, Listen: "127.0.0.1:0", Path: path}

			srv, err := NewServer(mcfg, newTestCollector(t, cfg, counter.New(cfg.Actions, true), &stubReader{}), discardLogger())
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
				t.Fatalf("NewServer(%q) must fail", path)
			}
			if !strings.Contains(err.Error(), "metrics.path") {
				t.Errorf("error = %v, want it to name metrics.path", err)
			}
		})
	}
}

// The bad path is caught before the socket is opened, so a failed start leaves
// nothing bound behind it.
func TestNewServerValidatesBeforeBinding(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	mcfg := config.Metrics{Enabled: true, Listen: addr, Path: "/met{rics"}
	if _, err := NewServer(mcfg, newTestCollector(t, cfg, counter.New(cfg.Actions, true), &stubReader{}), discardLogger()); err == nil {
		t.Fatal("NewServer must fail on a bad path")
	}

	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port was left bound by the failed NewServer: %v", err)
	}
	_ = again.Close()
}

// Shutdown has to release the port: the daemon stops the exporter before it
// exits, and a restart must not hit "address already in use".
func TestServerShutdownReleasesThePort(t *testing.T) {
	cfg := testConfig()
	mcfg := config.Metrics{Enabled: true, Listen: "127.0.0.1:0", Path: "/metrics"}
	srv, err := NewServer(mcfg, newTestCollector(t, cfg, counter.New(cfg.Actions, true), &stubReader{}), discardLogger())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	addr := srv.Addr()

	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	if code, _ := get(t, "http://"+addr+"/metrics"); code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// A clean shutdown is not an error for the caller of Serve.
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after a shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the shutdown")
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port is still taken after the shutdown: %v", err)
	}
	_ = ln.Close()
}
