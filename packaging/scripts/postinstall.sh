#!/bin/sh
# Runs after install and after upgrade, on both deb and rpm.
set -e

if ! getent passwd logstat >/dev/null 2>&1; then
    nologin=/bin/false
    for candidate in /usr/sbin/nologin /sbin/nologin; do
        if [ -x "$candidate" ]; then
            nologin="$candidate"
            break
        fi
    done
    useradd --system --no-create-home --home-dir /nonexistent \
            --shell "$nologin" logstat >/dev/null 2>&1 || true
fi

systemctl daemon-reload >/dev/null 2>&1 || true
# Bring instances that were running before an upgrade back on the new binary.
# try-restart is a no-op for units that are not running, so a fresh install
# starts nothing by itself.
systemctl try-restart 'logstat@*.service' >/dev/null 2>&1 || true

exit 0
