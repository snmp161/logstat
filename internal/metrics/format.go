package metrics

import (
	"fmt"
	"time"
)

// The exporter renders booleans twice, on purpose, and this is the one place
// that decides how:
//
//   - metrics follow the Prometheus convention — 1/0 for a value, "true"/"false"
//     for a label, because that is what queries and dashboards expect;
//   - the status page says "yes"/"no", mirroring the YAML the operator wrote
//     (`case_sensitive: yes`), so the page reads back like the config it came
//     from.
func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// setOrNot describes a value the page must not print, the Redis password.
func setOrNot(b bool) string {
	if b {
		return "set"
	}
	return "not set"
}

// humanTime is the timestamp format of the status page: readable at a glance
// and free of characters the HTML escaper would rewrite.
const humanTime = "2006-01-02 15:04:05 MST"

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

// formatTTL renders redis.ttl for the page, spelling out the disabled case.
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
