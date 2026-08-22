# Security policy

## Supported versions

The latest release. This project is in maintenance mode: no new features, but a
security fix will be released.

## Reporting a vulnerability

Please report privately rather than in a public issue:

- [GitHub security advisories](https://github.com/snmp161/logstat/security/advisories/new)
  (preferred), or
- the maintainer address published in the package metadata
  (`dpkg-deb --info logstat_*.deb | grep Maintainer`).

Please include the version (`logstat version`), the configuration with the Redis
password removed, and what you observed. Expect a reply within a couple of
weeks; this is a spare-time project.

## What is in scope

The daemon reads a log file, talks to Redis and — when `metrics.enabled` is on —
serves an HTTP endpoint. Anything that lets one of those cross a boundary it
should not (reading files it was not pointed at, leaking the Redis password,
turning log content into code) is in scope.

The metrics endpoint has no TLS, no authentication and no rate limiting by
design, and it discloses the configuration it runs with. That is documented, it
listens on loopback by default, and publishing it is the operator's decision —
so it is not a vulnerability on its own.
