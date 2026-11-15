#!/bin/sh
# mirafleet preremove: stop and disable the units on removal only (not on upgrade).
# deb prerm: remove|upgrade|deconfigure|failed-upgrade; rpm preun: 0 removal, 1 upgrade.
set -eu

case "${1:-}" in
upgrade | failed-upgrade | deconfigure | 1) exit 0 ;;
esac

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	systemctl disable --now mirafleet-central.service mirafleet-runner.service \
		mirafleet-runner-workspace.service >/dev/null 2>&1 || true
fi
