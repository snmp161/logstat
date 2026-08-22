package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/snmp161/logstat/internal/config"
)

// readHeaderTimeout bounds how long a client may take to send its request
// headers, so that an idle connection cannot pin a goroutine forever.
const readHeaderTimeout = 5 * time.Second

// Server is the HTTP endpoint serving the metrics of one daemon.
type Server struct {
	ln   net.Listener
	srv  *http.Server
	path string
	log  *slog.Logger
}

// NewServer binds the configured address right away and returns an error if it
// cannot: the daemon treats that as fatal, because running on without metrics
// that were explicitly switched on would leave the monitoring side watching
// silence and calling it health.
func NewServer(cfg config.Metrics, c *Collector, lg *slog.Logger) (*Server, error) {
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("metrics: listen %s: %w", cfg.Listen, err)
	}

	mux := http.NewServeMux()
	// A pattern without a trailing slash is an exact match, so everything but
	// the two paths below answers 404: a misconfigured scrape shows up as an
	// error instead of an empty success.
	mux.Handle(cfg.Path, promhttp.HandlerFor(NewRegistry(c), promhttp.HandlerOpts{
		ErrorLog:          slogErrorLog{lg: lg},
		ErrorHandling:     promhttp.ContinueOnError,
		EnableOpenMetrics: false,
	}))
	// The root is where a browser lands, so it gets the human-readable summary
	// — unless the exposition itself was configured to live there.
	if cfg.Path != "/" {
		mux.Handle("/{$}", c.statusHandler())
	}

	return &Server{
		ln:   ln,
		path: cfg.Path,
		log:  lg,
		srv: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: readHeaderTimeout,
		},
	}, nil
}

// Addr returns the address actually bound, which differs from the configured
// one when the port was left to the kernel.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve runs the endpoint until Shutdown is called. A clean shutdown is not an
// error.
func (s *Server) Serve() error {
	s.log.Info("metrics exporter listening", "addr", s.Addr(), "path", s.path)
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("metrics: serve: %w", err)
	}
	return nil
}

// Shutdown stops the endpoint and releases the port.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("metrics: shutdown: %w", err)
	}
	return nil
}

// slogErrorLog routes the internal errors of promhttp into the daemon log
// instead of the standard logger, which would bypass logging.output.
type slogErrorLog struct{ lg *slog.Logger }

func (l slogErrorLog) Println(v ...any) {
	l.lg.Warn("metrics endpoint error", "error", fmt.Sprint(v...))
}
