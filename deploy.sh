#!/bin/sh
# Build and install trough on this server (fedi.freifick.net).
# Installs: /usr/local/bin/trough, /usr/local/bin/troughctl, trough.service,
# /etc/trough.env (only if missing) and the Caddy site config.
set -eu
cd "$(dirname "$0")"

go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o build/trough ./cmd/trough

if ! id trough >/dev/null 2>&1; then
	sudo useradd --system --home-dir /var/lib/trough --shell /usr/bin/nologin trough
fi
sudo install -m 0755 build/trough /usr/local/bin/trough
sudo install -m 0755 deploy/troughctl /usr/local/bin/troughctl
[ -e /etc/trough.env ] || sudo install -m 0644 deploy/trough.env /etc/trough.env
sudo install -m 0644 deploy/trough.service /etc/systemd/system/trough.service
sudo systemctl daemon-reload
sudo systemctl enable trough.service >/dev/null 2>&1
sudo systemctl restart trough.service

if ! cmp -s deploy/fedi.freifick.net.caddy /etc/caddy/conf.d/fedi.freifick.net.caddy; then
	conf=/etc/caddy/conf.d/fedi.freifick.net.caddy
	sudo cp -p "$conf" "$conf.prev" 2>/dev/null || true
	sudo install -m 0644 deploy/fedi.freifick.net.caddy "$conf"
	# as caddy, which owns the log files the config opens
	if ! sudo -u caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1; then
		echo "new Caddy config is invalid; keeping the old one" >&2
		sudo mv "$conf.prev" "$conf"
		exit 1
	fi
	sudo rm -f "$conf.prev"
	sudo systemctl reload caddy
fi

sleep 1
systemctl is-active trough.service
curl -fsS -o /dev/null -w "local check: %{http_code}\n" -H "Host: fedi.freifick.net" http://127.0.0.1:8790/nodeinfo/2.0
