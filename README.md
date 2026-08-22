# logstat

**English** | [Русский](README.ru.md)

A daemon that follows a growing text log file, looks for the configured code words in every line and maintains incremental per-word counters in Redis. Each counter grows over time and is zeroed on a schedule (daily at midnight by default) — an external reader samples the value once per cycle. The self-reset can be switched off.

The daemon is tied neither to nginx nor to any log format: it works on raw lines and matches substrings. One process = one log file; several logs on a host are served by several instances of a template systemd unit.

Implemented in Go as a static binary (`CGO_ENABLED=0`), configured through YAML.

- [How it works](#how-it-works)
- [Redis keys](#redis-keys)
  - [Key expiry](#key-expiry)
- [Configuration](#configuration)
  - [Case sensitivity](#case-sensitivity)
- [CLI](#cli)
- [Building](#building)
- [Installing the package](#installing-the-package)
- [Multiple configs and instances](#multiple-configs-and-instances)
- [The daemon's own log](#the-daemons-own-log)
- [Read permissions on the log](#read-permissions-on-the-log)
- [Operator warnings](#operator-warnings)
- [Removing the package](#removing-the-package)
- [Tests](#tests)
- [License](#license)

## How it works

1. The daemon follows the log file the way `tail -F` does. An existing file is opened **at its end** (like `tail -n0`) — whatever was written before the start is not counted. If the file does not exist yet (the source software may come up later), the daemon does not fail: it waits for the file to appear and then reads it **from the beginning**.
2. Every new line is checked against each code word: **substring anywhere in the line**, case-sensitively by default and case-insensitively with `case_sensitive: no` ([details](#case-sensitivity)). Counting is **per line, not per occurrence** — a word occurring twice in one line still adds 1. Two different words in one line increment both.
3. Increments accumulate in memory and are flushed to Redis in one batch every `flush_interval` seconds instead of on every line. A flush issues `INCRBY` on the integer counter and then — if `heartbeat_key` is on — `SET`s the formatted heartbeat value with the new total taken from the `INCRBY` reply.
4. Log rotation is handled transparently in both logrotate modes: `create` (rename plus a new file, so the inode changes) and `copytruncate` (truncation in place). No line is lost and none is counted twice.
5. On the `reset.schedule` cron schedule the counters are zeroed: the rest of the buffer is flushed first (so the lines of the finishing interval are accounted for in it), then `SET counter 0` plus a heartbeat value of `lines=0`.
6. Redis may be unreachable, including a remote one — the buffer keeps growing in memory, the daemon neither dies nor loses counts, and it catches up once Redis is back. An outage is logged once (repetitions at `debug` level), and so is the recovery. Once the connection is known to be down, the flush stops walking the remaining words instead of waiting for a connection timeout on each: neither a flush nor a shutdown grows with the number of words. Internal `go-redis` messages also go into the daemon log (at `debug`) rather than to stderr.
7. On `SIGTERM`/`SIGINT` the rest of the buffer is flushed and the process exits with code 0.

The file offset is **deliberately not persisted** across restarts: the counter lives in Redis, so a restart does not lose what was accumulated, but lines written while the daemon was down are not counted retroactively.

## Redis keys

`<host>` is the short host name (the equivalent of `hostname -s`), `<action>` is a code word.

| Purpose | Key | Value | Operation | Condition |
|---|---|---|---|---|
| Internal counter | `logstat:counter:<host>:<action>` | integer | `INCRBY` / `SET 0` | always |
| Monitoring value (heartbeat) | `logstat:heartbeat:<host>:<action>` | `server=<host> time=<iso8601> type=<action> lines=<N>` | `SET` | `heartbeat_key: true` |

Both keys live for `redis.ttl` seconds, one day by default — see [Key expiry](#key-expiry).

An example heartbeat value:

```
server=web01 time=2026-08-19T15:04:05+03:00 type=get-sms lines=1274
```

The timestamp is ISO-8601 with a timezone offset, the format produced by `date -Iseconds`.

There are two keys because a formatted string cannot be incremented atomically: counting happens on the integer key, and the `<N>` of the heartbeat value comes from the `INCRBY` reply.

At startup missing keys are created with `SET ... NX` (the counter with zero, the heartbeat with the counter's current total) and existing ones are **left alone**: restarting the daemon does not reset what was accumulated. As a result the external reader sees the keys right after installation, before the first match.

**The heartbeat key can be switched off.** With `heartbeat_key: false` the daemon maintains the integer counters only: the heartbeat is not created at initialisation, not written on a flush and not touched by a reset, `logstat clear` included. An already existing key is **not deleted** when the option is turned off — the updates simply stop, and cleaning it up is up to you:

```sh
redis-cli --scan --pattern 'logstat:heartbeat:*' | xargs -r redis-cli del
```

**Migrating from v0.1.0.** In v0.1.0 the monitoring key was named `logstat:<host>_type=<action>`. Since v0.2.0 it is `logstat:heartbeat:<host>:<action>`: update the pattern in your reader and drop the old keys, which the daemon will not touch.

```sh
redis-cli --scan --pattern 'logstat:*_type=*' | xargs -r redis-cli del
```

The integer counters `logstat:counter:<host>:<action>` are unchanged — the accumulated values survive the upgrade.

### Key expiry

`redis.ttl` (seconds, `86400` — one day — by default, `0` for no expiry) sets how long both keys live. The TTL is **sliding**: it is re-applied on every flush, including a flush that has nothing to write.

That is deliberate, because `INCRBY` in Redis does not renew a TTL by itself: with a one-shot expiry the key of an actively used counter would die in the middle of a cycle and the count would silently restart from zero. So the TTL here does not measure how long a key exists, it measures **how long a key outlives the daemon**:

- while the instance runs, the keys never expire — the TTL is renewed every `flush_interval` seconds;
- once the instance is gone (host decommissioned, config removed, a word dropped from `actions`), its keys disappear on their own after `redis.ttl` instead of piling up in Redis as garbage.

`redis.ttl` has to be **longer** than `flush_interval`, otherwise the keys can expire between two flushes; validation emits a warning if it is not.

With `redis.ttl: 0` no expiry is set, and an expiry left over from an earlier configuration is removed: the daemon issues a one-off `PERSIST` at startup, so turning the option off is reversible without manual cleanup.

## Configuration

The config path comes from `--config /etc/logstat/config.yaml` (short form `-c`). Defaults are built in: an absent field falls back to its default. An example carrying every field is [`packaging/default.yaml`](packaging/default.yaml).

```yaml
log_path: /var/log/app.log

actions:
  - get-number
  - get-sms
  - getNumber
  - getStatus

case_sensitive: yes
flush_interval: 10
poll: false
heartbeat_key: true
lock_file: /run/logstat/default/logstat.lock

redis:
  host: 127.0.0.1
  port: 6379
  db: 0
  password: ""
  ttl: 86400

logging:
  level: info
  output: journald
  file: ""

reset:
  enabled: true
  schedule: "0 0 * * *"
```

| Parameter | Type | Default | Purpose |
|---|---|---|---|
| `log_path` | string | `/var/log/app.log` | path to the watched log file |
| `actions` | list\<string\> | `[get-number, get-sms, getNumber, getStatus]` | code words |
| `case_sensitive` | bool | `true` | match the code words case-sensitively; `no` matches any case |
| `flush_interval` | int (sec) | `10` | how often the buffer is flushed to Redis |
| `poll` | bool | `false` | poll instead of using inotify (for network file systems) |
| `heartbeat_key` | bool | `true` | maintain the heartbeat key `logstat:heartbeat:<host>:<action>` |
| `lock_file` | string | `/run/logstat/logstat.lock` | lock file of the instance (unique per config) |
| `redis.host` | string | `127.0.0.1` | Redis host (may be remote) |
| `redis.port` | int | `6379` | Redis port |
| `redis.db` | int | `0` | Redis database number |
| `redis.password` | string | `""` | `AUTH`, if `requirepass` is set |
| `redis.ttl` | int (sec) | `86400` | key expiry, sliding; `0` for no expiry |
| `logging.level` | string | `info` | `debug` / `info` / `warn` / `error` |
| `logging.output` | string | `journald` | `journald` (stderr) or `file` |
| `logging.file` | string | `""` | log file path when `output: file` (unique per config) |
| `reset.enabled` | bool | `true` | enable the self-reset |
| `reset.schedule` | string | `"0 0 * * *"` | cron schedule of the reset (standard 5-field) |

### Case sensitivity

`case_sensitive` decides how the substring search compares the code words with the line. Being a YAML boolean, it accepts `yes`/`no` as readily as `true`/`false`.

| Value | Behaviour |
|---|---|
| `yes` (default) | byte-for-byte comparison: `get-number` matches `get-number` only, not `GET-NUMBER` and not `Get-Number` |
| `no` | the line and the code words are lowercased before the search, so every spelling of a word feeds the same counter |

```yaml
actions:
  - get-number
case_sensitive: no
```

With that config all three lines below add 1 to the counter of `get-number`:

```
"GET /api/get-number?x=1 HTTP/1.1" 200
"GET /api/GET-NUMBER?x=1 HTTP/1.1" 200
"GET /api/Get-Number?x=1 HTTP/1.1" 200
```

The default is `yes`, so an existing config keeps the behaviour it had before this option existed.

Case folding affects **matching only**. The Redis keys and the heartbeat value always spell the word the way `actions` does, never the way the line does, so `logstat:counter:<host>:get-number` stays the one key of that word whichever spelling was in the log — and flipping the option does not move a running count to a different key.

Two consequences worth knowing before switching the option off:

- Prefix overlaps are then judged without case too, so a pair like `getstatus` / `GetStatusExtended` starts warning at startup although it is silent in the case-sensitive mode.
- Words that differ **only** in case (`getNumber` and `GETNUMBER`) always match together but keep two separate Redis keys — almost certainly a config mistake. The daemon warns about such a pair and keeps both words: dropping one of them would silently abandon a key that a reader may already be sampling.

**The reset schedule.** `reset.schedule` is a standard 5-field cron expression (`minute hour day-of-month month day-of-week`), interpreted in the host's local timezone, always firing at `:00` seconds of the target minute. There is no hand-written time logic, so the flexibility comes for free:

| Expression | Meaning |
|---|---|
| `0 0 * * *` | daily at midnight (default) |
| `1 * * * *` | the first minute of every hour |
| `*/30 * * * *` | every half hour |
| `0 */6 * * *` | every six hours |

Descriptors such as `@daily` and `@hourly` are accepted too. The six-field syntax with seconds is deliberately **not** accepted.

With `reset.enabled: false` the daemon only counts; the zeroing is done by something else (an external script or timer, or a manual `logstat clear`).

**Validation at startup.** The config is parsed strictly: an unknown field is an error. The daemon checks that `actions` is not empty, that `flush_interval > 0`, that `lock_file` is set and its directory can be created, that `reset.schedule` is a valid 5-field cron expression, that `logging.level` and `logging.output` are from the allowed sets, and that with `output: file` the `logging.file` field is non-empty and the path writable. The values of `poll`, `heartbeat_key` and `case_sensitive` must be booleans and `redis.ttl` must not be negative. A fatal config error exits with a non-zero code, which systemd shows in the unit status. Duplicates in `actions`, words that are a substring of another one and — with `case_sensitive: no` — words differing only in case produce a warning in the log but do not prevent the start.

## CLI

```
logstat run   --config <path>                 run the daemon (main mode)
logstat clear --config <path> --all           zero every counter of the config
logstat clear --config <path> --action <word> zero a single counter
logstat version                               version, commit, build date
logstat --help                                help
```

`clear` works at any time, regardless of `reset.enabled`, and zeroes both the integer counter and the heartbeat value (`lines=0`; with `heartbeat_key: false` the heartbeat is left alone). `--action` only accepts a word listed in the `actions` of that config, so a typo cannot create a stray key. It is matched against the config exactly, `case_sensitive` notwithstanding: the option governs the search in the log, not the key names.

Exit codes: `0` success, `1` runtime failure (config, lock, Redis), `2` usage error.

## Building

Go is required; the version is pinned in `go.mod`. `CGO_ENABLED=0` is set automatically.

```sh
make build          # static binary for the host architecture → dist/logstat
make build-all      # linux/amd64 + linux/arm64 → dist/logstat_linux_<arch>/logstat
make test           # go test -race ./...
make cover          # tests with a coverage profile
make lint           # gofmt + go vet + golangci-lint
make package        # .deb and .rpm for both architectures (needs nfpm)
make tools          # install nfpm and golangci-lint into GOPATH/bin
make clean
make help
```

The version, commit and build date are baked in through `-ldflags` and shown by `logstat version`. By default the version comes from `git describe`; it can be set explicitly: `make build VERSION=v1.2.3`.

Packages are built with [nfpm](https://github.com/goreleaser/nfpm) from the compiled binary — a single [`nfpm.yaml`](nfpm.yaml) covers both formats. Releases are built by GoReleaser from [`.goreleaser.yaml`](.goreleaser.yaml): on a `v*` tag push the [`release.yml`](.github/workflows/release.yml) workflow publishes a GitHub Release with the binaries, the `.deb`, the `.rpm` and checksums. On every push and pull request the [`ci.yml`](.github/workflows/ci.yml) workflow runs lint, `go test -race` with coverage, the cross build and the package build.

## Installing the package

```sh
# Debian / Ubuntu
sudo dpkg -i logstat_<version>-1_amd64.deb

# RHEL / Alma / Rocky
sudo rpm -i logstat-<version>-1.x86_64.rpm
```

The package installs:

- `/usr/bin/logstat` — the static binary;
- `/etc/logstat/default.yaml` — the example config as a conffile / `%config(noreplace)`, so an upgrade does not overwrite local edits;
- `/lib/systemd/system/logstat@.service` (on rpm distributions `/usr/lib/systemd/system/logstat@.service`) — the template unit;
- postinstall creates the system user `logstat`, runs `systemctl daemon-reload` and `try-restart 'logstat@*.service'` so that instances which were running before an upgrade come back on the new binary.

Next comes a config per log file and starting the instance:

```sh
sudoedit /etc/logstat/app1.yaml
sudo systemctl enable --now logstat@app1.service
systemctl status 'logstat@*'
journalctl -u logstat@app1 -f
```

The unit deliberately depends on neither Redis nor nginx (only on `network-online.target`): Redis may live on another machine, the in-memory buffer survives its unavailability, and the log source is arbitrary. With `reset.enabled: true` no timers or clear services are needed.

## Multiple configs and instances

The unit is a template: the instance `logstat@<name>` reads `/etc/logstat/<name>.yaml`. So `logstat@app1` parses `/etc/logstat/app1.yaml` and `logstat@nginx` parses `/etc/logstat/nginx.yaml`. The single-log case is just one instance, for example `logstat@default` with `/etc/logstat/default.yaml`.

With several instances on one host:

- every config needs its **own** `lock_file`, otherwise the second instance will not start because of `flock`. The unit gives each instance its own runtime directory (`RuntimeDirectory=logstat/%i` → `/run/logstat/<instance>`), which makes `/run/logstat/<instance>/logstat.lock` the natural path. It is also the only directory the daemon can write to under `ProtectSystem=strict`;
- with `logging.output: file`, every instance needs its **own** `logging.file`;
- if the instances point at the **same** Redis (host/port/db), their `actions` sets must not overlap, otherwise the keys collide (see [Operator warnings](#operator-warnings)). A different Redis or a different `db` settles the question by itself.

Rollout goes either through Salt (`file.managed` for the package and the per-instance configs plus `service.running` over the list of instances) or by installing the `.deb` / `.rpm` from a GitHub Release.

## The daemon's own log

The daemon keeps its own log: start and stop, the loaded config, the fact and the result of every flush, a reset firing, Redis being unavailable and reconnecting, log rotation, errors. The format is human readable — timestamp, level, message, attributes:

```
2026-08-19T15:04:05+03:00 INFO  starting log_path=/var/log/app.log actions=4 flush_interval=10 ...
2026-08-19T15:04:15+03:00 WARN  redis unavailable, buffering in memory op=incrby error="..."
```

Events about individual log lines are only written at `debug` level — at a high request rate they are far too noisy.

- `logging.output: journald` (the default) writes to **stderr**, from where systemd collects it into journald: `journalctl -u logstat@<instance>`. No separate journald protocol integration is required.
- `logging.output: file` writes to the file given by `logging.file`. Rotating that file is the job of an external logrotate; the daemon reopens its log on `SIGHUP`, which is convenient to wire up as `postrotate systemctl reload logstat@<instance>` (the unit carries `ExecReload=/bin/kill -HUP $MAINPID`).

  Note that under `ProtectSystem=strict` the `/var/log` directory is read-only. To write there, add `LogsDirectory=logstat` to the unit, preferably through a drop-in: systemd will create a writable `/var/log/logstat`.

  ```sh
  sudo systemctl edit logstat@app1.service
  # [Service]
  # LogsDirectory=logstat
  ```

## Read permissions on the log

The daemon runs as the unprivileged user `logstat` with systemd hardening (`NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`). Which group is needed to read the log depends on the file's owner: for nginx it is usually `adm`, which is what the unit ships with; for other software `SupplementaryGroups` has to be adjusted to the specific log — sometimes plain permissions are enough, if the file is world readable.

```sh
ls -l /var/log/app.log            # check the owner and the group
sudo systemctl edit logstat@app1.service
# [Service]
# SupplementaryGroups=
# SupplementaryGroups=<the required group>
```

## Operator warnings

**Prefix overlaps in `actions`.** Matching is substring based, so a word that is part of another word from the list increments both counters: `getStatus` also matches inside a hypothetical `getStatusExtended`. The four default words do not overlap (none of them is a substring of another). Adding new words calls for a check — at startup the daemon logs a warning for every such pair, but it does not refuse to run. With [`case_sensitive: no`](#case-sensitivity) the overlap is judged without case, which turns pairs like `getstatus` / `GetStatusExtended` into overlaps as well.

**A possible key collision.** The keys contain only `<host>` and `<action>`. A collision is possible in exactly one scenario: several instances on one host configured with the **same** Redis (host/port/db) **and** overlapping `actions` — then they write to a shared key and overwrite each other. This is normally not the case: different configs usually mean a different Redis (its own host/db in each), and if Redis really is shared, non-overlapping `actions` sets are enough. The software does **not** prevent this case; it is the operator's responsibility.

**Lines written while the daemon was down are not counted.** The file offset is deliberately not persisted across restarts, for the sake of simplicity: after a restart the daemon positions itself at the end of the file and the counter continues from its current value in Redis.

**Rotation in `copytruncate` mode is inherently racy.** Between the copy and the truncation the source software can write lines that are lost for every reader. If rotation is under your control, `create` mode is preferable.

## Removing the package

The instance enable symlinks (`/etc/systemd/system/*.wants/logstat@<name>.service`) are created by the administrator through `systemctl enable`, do not belong to the package and would not be removed by themselves. Therefore:

- prerm stops every running instance (`systemctl stop 'logstat@*.service'`) — only on an actual removal; on an upgrade the instances are left alone and are brought back by postinstall;
- on removal, postrm walks the `.wants` directories, drops the orphaned symlinks and runs `daemon-reload`.

For a clean removal it is still better to do it yourself beforehand:

```sh
sudo systemctl disable --now logstat@app1.service
sudo apt-get remove logstat     # or: sudo rpm -e logstat
```

The `logstat` user and the `/etc/logstat/*.yaml` files created by the administrator are left in place.

## Tests

```sh
make test     # go test -race ./...
make cover    # plus a coverage profile and the resulting number
```

No external services are needed: Redis is replaced by [miniredis](https://github.com/alicebob/miniredis) and log files are created in temporary directories. The suite covers matching (substring, both case modes, per-line counting, prefix overlaps), the config parser and validator (including the `case_sensitive` default, its warnings and an end-to-end run in the case-insensitive mode), key and value formatting, assembling `lines=<N>` from the `INCRBY` reply, the `heartbeat_key: false` mode, setting and renewing the TTL on both keys (including an abandoned key expiring and the expiry being cleared with `ttl: 0`), computing the next firing time of cron schedules, tailing across both rotation schemes, starting with the log file absent, behaviour with Redis unavailable and catching up after recovery, resetting the counters (both directly and through the scheduler on an every-second schedule, without waiting for real cron), log level filtering and log file reopening, single-instance enforcement through `flock`, and a graceful shutdown on a real `SIGTERM` in a separate process.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
