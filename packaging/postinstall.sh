#!/bin/sh
# After install or upgrade: the ibc user, then the service (started, or restarted
# to run the new program). The node's data in /var/lib/ibc is kept.
set -e

if ! getent passwd ibc >/dev/null; then
    adduser --system --group --home /var/lib/ibc --no-create-home --shell /usr/sbin/nologin ibc
fi

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    systemctl enable ibc >/dev/null 2>&1 || true
    systemctl restart ibc
fi
