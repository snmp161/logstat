package metrics

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"
)

// missing is what a word whose Redis key does not exist shows instead of a
// zero, the same way it is a missing series in the metrics.
const missing = "—"

// statusData is what the status page renders. Everything is preformatted here
// so that the template stays a layout and nothing else.
type statusData struct {
	Version     string
	Host        string
	Uptime      string
	Started     string
	Rendered    string
	MetricsPath string

	RedisAddr string
	RedisDB   int
	RedisUp   bool

	Words  []wordRow
	Config [][2]string
}

type wordRow struct {
	Action  string
	Matched int64
	Pending int64
	Redis   string
}

// status collects the same numbers the metrics carry, for a human to read.
// A failed Redis read is part of the page rather than an error page: during an
// outage what this process counted is exactly what somebody wants to look at.
func (c *Collector) status() statusData {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	counters, err := c.reader.Counters(ctx, c.cfg.Actions)
	redisUp := err == nil

	matched := c.cnt.Matched()
	pending := c.cnt.PendingByAction()

	words := make([]wordRow, 0, len(c.cfg.Actions))
	for _, a := range c.cfg.Actions {
		row := wordRow{Action: a, Matched: matched[a], Pending: pending[a], Redis: missing}
		if n, ok := counters[a]; ok {
			row.Redis = strconv.FormatInt(n, 10)
		}
		words = append(words, row)
	}

	now := c.now()
	cfg := c.cfg
	return statusData{
		Version: c.version,
		Host:    c.reader.Host(),
		Uptime:  formatUptime(now.Sub(c.start)),
		// A human-readable stamp rather than RFC 3339: the page is read, not
		// parsed, and the offset of RFC 3339 comes out HTML-escaped.
		Started:     c.start.Format(humanTime),
		Rendered:    now.Format(humanTime),
		MetricsPath: cfg.Metrics.Path,
		RedisAddr:   cfg.Redis.Addr(),
		RedisDB:     cfg.Redis.DB,
		RedisUp:     redisUp,
		Words:       words,
		Config: [][2]string{
			{"log_path", cfg.LogPath},
			{"lock_file", cfg.LockFile},
			{"case_sensitive", yesNo(cfg.CaseSensitive)},
			{"flush_interval", fmt.Sprintf("%ds", cfg.FlushInterval)},
			{"poll", yesNo(cfg.Poll)},
			{"heartbeat_key", yesNo(cfg.HeartbeatKey)},
			{"redis.ttl", formatTTL(cfg.Redis.TTL)},
			// The password itself never leaves the process, here as elsewhere.
			{"redis.password", setOrNot(cfg.Redis.Password != "")},
			{"reset", formatReset(cfg.Reset.Enabled, cfg.Reset.Schedule)},
			{"logging", formatLogging(cfg.Logging.Level, cfg.Logging.Output, cfg.Logging.File)},
			{"metrics.listen", cfg.Metrics.Listen},
		},
	}
}

// statusHandler renders the page. It is mounted on the root of the exporter,
// which is where a browser lands.
func (c *Collector) statusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := statusTemplate.Execute(w, c.status()); err != nil {
			// The status line is already sent, so there is nothing to report to
			// the client; the daemon log is the place for it.
			c.log.Warn("cannot render the status page", "error", err)
		}
	})
}

// formatUptime renders a duration the way a person reads it: the two units that
// matter and no more.
func formatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// humanTime is the timestamp format of the page: readable at a glance and free
// of characters the HTML escaper would rewrite.
const humanTime = "2006-01-02 15:04:05 MST"

func setOrNot(b bool) string {
	if b {
		return "set"
	}
	return "not set"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func formatTTL(seconds int) string {
	if seconds == 0 {
		return "no expiry"
	}
	return (time.Duration(seconds) * time.Second).String()
}

func formatReset(enabled bool, schedule string) string {
	if !enabled {
		return "disabled (zeroed externally)"
	}
	return schedule
}

func formatLogging(level, output, file string) string {
	if file != "" {
		return fmt.Sprintf("%s → %s (%s)", level, output, file)
	}
	return fmt.Sprintf("%s → %s", level, output)
}

// statusTemplate is deliberately one self-contained page: no assets to serve,
// no scripts to run, nothing to configure.
var statusTemplate = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>logstat · {{.Host}}</title>
<style>
:root { color-scheme: light dark; }
body { font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
       margin: 2rem auto; max-width: 46rem; padding: 0 1rem; }
h1 { font-size: 1.1rem; margin: 0; display: inline; }
h2 { font-size: .95rem; margin: 2rem 0 .5rem; font-weight: 600; opacity: .7; }
header { display: flex; justify-content: space-between; align-items: baseline;
         border-bottom: 1px solid currentColor; padding-bottom: .5rem; }
.dim { opacity: .6; }
.ok { color: light-dark(#128a35, #4ec97a); }
.bad { color: light-dark(#c02626, #ff7b72); font-weight: 600; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: right; padding: .25rem .5rem; }
th:first-child, td:first-child { text-align: left; }
thead th { font-weight: 600; opacity: .7; border-bottom: 1px solid currentColor; }
/* The word column takes the slack so that the numbers stay together. */
th:first-child, td:first-child { width: 100%; }
th + th, td + td { white-space: nowrap; }
tbody tr:nth-child(even) { background: rgba(127,127,127,.12); }
.cfg td { text-align: left; }
.cfg td:first-child { opacity: .7; width: 12rem; }
footer { margin-top: 2rem; }
</style>
</head>
<body>
<header>
  <div><h1>logstat</h1> <span class="dim">{{.Version}} · {{.Host}}</span></div>
  <a href="{{.MetricsPath}}">{{.MetricsPath}} →</a>
</header>

<p>up {{.Uptime}} <span class="dim">(since {{.Started}})</span> ·
redis {{.RedisAddr}} db {{.RedisDB}} ·
{{if .RedisUp}}<span class="ok">up</span>{{else}}<span class="bad">unreachable</span>{{end}}</p>

<h2>code words</h2>
<table>
  <thead><tr><th>word</th><th>in memory</th><th>buffered</th><th>in redis</th></tr></thead>
  <tbody>
  {{range .Words}}<tr><td>{{.Action}}</td><td>{{.Matched}}</td><td>{{.Pending}}</td><td>{{.Redis}}</td></tr>
  {{end}}</tbody>
</table>
<p class="dim">in memory: matched since this process started · buffered: waiting for the next flush ·
in redis: the shared total the external reader sees, {{.MetricsPath}} carries the same numbers.
— means the key does not exist.</p>

<h2>configuration</h2>
<table class="cfg"><tbody>
{{range .Config}}<tr><td>{{index . 0}}</td><td>{{index . 1}}</td></tr>
{{end}}</tbody></table>

<footer class="dim">rendered {{.Rendered}} · reload to refresh</footer>
</body>
</html>
`))
