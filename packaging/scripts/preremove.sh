#!/bin/sh
# Runs before the package is removed and, on upgrades, before the old version is
# replaced. $1 is "remove"/"upgrade" on deb and the number of remaining versions
# on rpm ("0" means the package is going away).
set -e

case "$1" in
    remove|purge|0)
        # Stop every instance of the template unit; the glob works for loaded units.
        systemctl stop 'logstat@*.service' >/dev/null 2>&1 || true
        ;;
esac

exit 0
