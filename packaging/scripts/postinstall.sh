#!/bin/sh
# mirafleet postinstall. Idempotent. Never enables a unit; never deletes anything.
# $1/$2: deb "configure" [old-version]; rpm 1 (install) or 2 (upgrade); apk none.
set -eu

install -d -m 0755 -o root -g root /var/lib/mirafleet
install -d -m 0700 -o mirafleet -g mirafleet /var/lib/mirafleet/central /var/lib/mirafleet/runner
install -d -m 0700 -o root -g root /var/lib/mirafleet/runner-ws /var/lib/mirafleet/ws

# The package ships central.env as 0600 root; enforce it when a config file survived an older
# layout. Never touch a file the admin removed.
if [ -f /etc/mirafleet/central.env ]; then
	chown root:root /etc/mirafleet/central.env
	chmod 0600 /etc/mirafleet/central.env
fi

upgrade=0
if [ "${1:-}" = configure ] && [ -n "${2:-}" ]; then upgrade=1; fi
if [ "${1:-}" = 2 ]; then upgrade=1; fi

if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload || true
	if [ "$upgrade" = 1 ]; then
		# Restarts only units that are already running; migrations take an automatic backup.
		# User units (systemctl --user) are not reachable from here: users restart those themselves.
		systemctl try-restart mirafleet-central.service mirafleet-runner.service \
			mirafleet-runner-workspace.service 'mirafleet-runner@*.service' || true
	fi
fi

if [ "$upgrade" = 0 ]; then
	cat <<'MSG'
mirafleet installed. The units are not enabled; start central with:
  systemctl enable --now mirafleet-central
Firewall: port 2222 must reach runners and operators. The Admin GUI listens on all
interfaces (0.0.0.0:8443) by default: allow 8443 only from admins' networks, or set
-gui-listen 127.0.0.1:8443 in /etc/mirafleet/central.env and use ssh -L.
Docs: /usr/share/doc/mirafleet/
MSG
fi
