#!/bin/sh
# mirafleet preinstall: create the system group and user. Idempotent; never removes anything.
set -eu

if ! getent group mirafleet >/dev/null 2>&1; then
	if command -v groupadd >/dev/null 2>&1; then
		groupadd --system mirafleet
	else
		addgroup -S mirafleet
	fi
fi

if ! getent passwd mirafleet >/dev/null 2>&1; then
	nologin=/bin/false
	for p in /usr/sbin/nologin /sbin/nologin; do
		if [ -x "$p" ]; then
			nologin=$p
			break
		fi
	done
	if command -v useradd >/dev/null 2>&1; then
		useradd --system --gid mirafleet --no-create-home --home-dir /var/lib/mirafleet \
			--shell "$nologin" --comment "mirafleet" mirafleet
	else
		adduser -S -D -H -h /var/lib/mirafleet -s "$nologin" -G mirafleet -g mirafleet mirafleet
	fi
fi
