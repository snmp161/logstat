# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`logstat` is a Go daemon that tails one line-oriented log file, counts configured code words per line and keeps incremental counters in Redis. One process = one log file; several logs on a host mean several instances of the template systemd unit `logstat@<name>`, each with its own `/etc/logstat/<name>.yaml`.

## Commands

```sh
make build        # static binary into dist/ (CGO_ENABLED=0, version via ldflags)
make test         # full suite with -race
make test-short   # skips the slow integration tests (they tail real files for seconds)
make cover        # coverage profile plus a total
make fuzz         # fuzz config.Parse + Validate, FUZZTIME=1m
make soak         # scripts/soak.sh: real log, real Redis, real clock, DURATION=10m
make lint         # gofmt -l, go vet, golangci-lint (make tools installs the pinned tools)
make package      # .deb and .rpm through nfpm
```

Tool versions are pinned in the Makefile (`GOLANGCI_VERSION`, `NFPM_VERSION`) and
repeated in the workflows, and the linter set is fixed in `.golangci.yml`. Keep
them in step, and keep them buildable with the Go version in `go.mod`: the newest
golangci-lint and nfpm already require a newer toolchain than this project pins.

`make test` fails locally with "-race requires cgo" because the Makefile exports `CGO_ENABLED=0` for the static build. Run the race suite directly:

```sh
CGO_ENABLED=1 go test -race -timeout 10m ./...
go test ./internal/metrics/ -run TestStatusPageOnTheRoot -v   # single test
```

No external services are needed: Redis is `miniredis`, log files are created in `t.TempDir()`.

CI runs lint, `go test -race`, a build matrix (linux/amd64 + arm64), packaging, and two jobs that install what was packaged — the `.deb` on the runner itself (systemd, template unit, upgrade over itself, removal with the maintainer scripts checked) and the `.rpm` in a Fedora container. It also runs weekly on a schedule, as a canary for a repository that may sit untouched for months. A `v*` tag is what triggers a release (GoReleaser); pushing to `main` does not.

The project is in maintenance mode: no new features, and the surface deliberately does not grow. Before proposing one, read the non-goals in the specification.

## Architecture

The flow is `tail → counter → flush → Redis`, with everything wired in `internal/daemon`:

- **`internal/config`** — YAML with built-in defaults, decoded with `KnownFields(true)` so a typo is a fatal error. `Validate()` is pure, returns `(warnings, error)` and runs every check before returning, so a config with three typos reports all three at once; `CheckPaths()` does the filesystem-touching part. Fields of an optional feature are validated **even when the feature is off** (`reset.schedule`, `metrics.listen`, `metrics.path`), so typos surface on the day they are written, not the day someone flips the flag. `Metrics.ValidatePath` is exported because the exporter repeats it: the path reaches an `http.ServeMux`, which *panics* on a pattern it cannot parse.
- **`internal/counter`** — the matching rule and the in-memory buffer. It keeps three things apart: `pending` (drained by each flush), `matched` (monotonic per action since start, what Prometheus needs) and `lines`. Case-insensitive mode pre-folds the needles and lowercases the line once per line, and increments are always keyed by the action **as spelled in `actions`**, never as it appeared in the log.
- **`internal/store`** — the Redis key schema (`logstat:counter:<host>:<action>`, optional `logstat:heartbeat:<host>:<action>`) and every operation on it. The TTL is sliding: re-applied on each flush, including empty ones, because `INCRBY` does not renew it. `Counters()` is the read side used by the exporter; a key that does not exist is absent from the result rather than zero, and a key holding something unparsable is skipped the same way and reported as `ErrMalformedValue` — which `IsUnavailable` deliberately classifies as *not* an outage.
- **`internal/daemon`** — `Run` binds the metrics socket, starts the tailer, then loops over a flush ticker, the cron reset channel and `ctx.Done()`. Failed writes are not lost: increments go back into the buffer via `Restore`, and `pendingHeartbeats` / `pendingResets` / `initialized` are retried on the next flush. Once `store.IsUnavailable` says the connection is down, the flush stops walking the remaining actions instead of paying a dial timeout per word.
- **`internal/metrics`** — a `prometheus.Collector` that renders the daemon state per scrape (Redis is read *during* the scrape, one `MGET` with a 2 s timeout), an HTTP server, and a status page on the root of the same port.
- **`internal/logging`** — human-readable `slog` handler writing to stderr (journald) or to a file reopened on `SIGHUP` after logrotate.
- **`internal/lockfile`** — `flock(2)` on the per-instance `lock_file`, the guard against a manual double start.

Invariants that are easy to break and hard to notice:

- The Redis password never leaves the process — not in a metric label, not on the status page. Only "set / not set".
- A missing Redis key is a missing series, never a `0`: an invented zero is indistinguishable from a counter that was honestly zeroed.
- A Redis outage is data, not an error: the scrape still succeeds with `logstat_redis_up 0`. `redis_up` answers only "did Redis reply" — an unreadable value in one key leaves it at `1`, drops that one series and bumps `logstat_redis_scrape_errors_total`.
- The exporter failing is fatal, at startup (a taken port) and later (the HTTP server dying, delivered to the `Run` loop through `metricsErr`) — running without metrics somebody explicitly enabled would show monitoring silence and let it read as health.
- Labels on `logstat_config_info` are deliberately few: each one is a new time series when it changes, and the rest of the configuration is on the status page for free.
- Per-line events are logged at `debug` only; at a high request rate anything else floods the log.
- With several instances on one host, `lock_file`, `logging.file` and `metrics.listen` have to differ per config, and instances sharing a Redis must not share `actions`.

## Working in this repo

- `docs/specification.md` (Russian) is the source of truth for behaviour and carries the reasoning behind each decision, including an "Appendix A: fixed decisions" table. Update it when behaviour changes.
- `README.md` (English, primary) and `README.ru.md` are a translation pair — a change to one belongs in the other in the same commit, including the parameter tables and the config example.
- `packaging/default.yaml` is the shipped example config and documents every field in comments; new options belong there too.
- The established order for a feature is **documentation → tests → code**: the specification and both READMEs first, then the failing tests, then the implementation.
- Comments explain *why*, not *what*, and tests carry a comment naming the behaviour they pin down. Match that tone rather than adding narration.
- Config options that change existing behaviour default to the old behaviour (see `case_sensitive`, `heartbeat_key`, `metrics.enabled`), so that an upgrade never surprises an installed instance.
