#!/bin/sh
set -e

SERVICE="csp-web.service"
DATA_DIR="/var/lib/csp-web"
USER="csp-check"
GROUP="csp-check"

if ! getent group "$GROUP" >/dev/null 2>&1; then
  groupadd --system "$GROUP" >/dev/null 2>&1 || true
fi
if ! id -u "$USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent \
    --shell /usr/sbin/nologin --gid "$GROUP" "$USER" >/dev/null 2>&1 || true
fi

install -d -m0755 -o "$USER" -g "$GROUP" "$DATA_DIR"

DEFAULT_FILE="/etc/default/csp-web"
if [ -f "$DEFAULT_FILE" ]; then
  chown root:root "$DEFAULT_FILE" >/dev/null 2>&1 || true
  chmod 0644 "$DEFAULT_FILE" >/dev/null 2>&1 || true
fi

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
  systemctl enable --now "$SERVICE" >/dev/null 2>&1 || true
fi

exit 0
