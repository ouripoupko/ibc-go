#!/bin/sh
# Before removal (not before an upgrade, whose postinstall restarts it): stop the service.
set -e

if [ "$1" = remove ] && [ -d /run/systemd/system ]; then
    systemctl disable --now ibc >/dev/null 2>&1 || true
fi
