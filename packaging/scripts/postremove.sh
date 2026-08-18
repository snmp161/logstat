#!/bin/sh
# Runs after the package has been removed. $1 is "remove"/"purge"/"upgrade" on
# deb and the number of remaining versions on rpm ("0" means it is gone).
set -e

case "$1" in
    remove|purge|0)
        # Instance enable symlinks were created by the administrator
        # (systemctl enable logstat@<name>), do not belong to the package and
        # would dangle once the template unit is gone. The *.wants glob catches
        # instances in any target, not just multi-user.
        for link in /etc/systemd/system/*.wants/logstat@*.service; do
            [ -L "$link" ] || continue   # no match: the glob stays literal, the guard drops it
            rm -f "$link"
        done
        systemctl daemon-reload >/dev/null 2>&1 || true
        ;;
esac

exit 0
