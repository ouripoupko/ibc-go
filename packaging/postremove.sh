#!/bin/sh
# After removal. The node's data (/var/lib/ibc: its key, agents and contracts)
# and the ibc user are kept even on purge: deleting a node's key cannot be undone.
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
