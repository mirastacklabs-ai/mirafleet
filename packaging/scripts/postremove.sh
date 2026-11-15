#!/bin/sh
# mirafleet postremove. Never deletes /var/lib/mirafleet, /etc/mirafleet or the user, also
# on purge: data and credentials are the admin's to remove.
set -eu

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
