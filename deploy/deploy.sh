#!/usr/bin/env bash
# Build for arm64 and install on a Debian host as a systemd service behind `tailscale serve`.
# Usage: DEPLOY_HOST=user@host deploy/deploy.sh   (first run also needs data/config.json and .env locally)
set -euo pipefail
: "${DEPLOY_HOST:?set DEPLOY_HOST=user@host}"
PORT="${PORT:-8095}"            # local port on the host
HTTPS_PORT="${HTTPS_PORT:-9443}" # tailnet HTTPS port
cd "$(dirname "$0")/.."

GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/voice-expenses-arm64 .
ssh "$DEPLOY_HOST" 'mkdir -p ~/voice-expenses/data && chmod 700 ~/voice-expenses'
scp -q /tmp/voice-expenses-arm64 "$DEPLOY_HOST:voice-expenses/voice-expenses.new"
# Config and secrets only on first install; the host's copies win afterwards.
ssh "$DEPLOY_HOST" 'test -f ~/voice-expenses/.env' || scp -q .env "$DEPLOY_HOST:voice-expenses/.env"
ssh "$DEPLOY_HOST" 'test -f ~/voice-expenses/data/config.json' || scp -q data/config.json "$DEPLOY_HOST:voice-expenses/data/config.json"
ssh "$DEPLOY_HOST" 'test -f ~/voice-expenses/data/terms.txt' || { test -f data/terms.txt && scp -q data/terms.txt "$DEPLOY_HOST:voice-expenses/data/terms.txt"; }

ssh "$DEPLOY_HOST" PORT="$PORT" HTTPS_PORT="$HTTPS_PORT" bash -s <<'REMOTE'
set -euo pipefail
cd ~/voice-expenses
chmod 600 .env
mv voice-expenses.new voice-expenses
sudo tee /etc/systemd/system/voice-expenses.service >/dev/null <<UNIT
[Unit]
Description=voice-expenses
After=network-online.target
Wants=network-online.target

[Service]
User=$USER
WorkingDirectory=$HOME/voice-expenses
EnvironmentFile=$HOME/voice-expenses/.env
Environment=ADDR=127.0.0.1:$PORT DATA_DIR=$HOME/voice-expenses/data
ExecStart=$HOME/voice-expenses/voice-expenses
Restart=on-failure
MemoryMax=128M

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable --now voice-expenses
sudo systemctl restart voice-expenses
sudo tailscale serve --bg --https="$HTTPS_PORT" "http://127.0.0.1:$PORT" >/dev/null
sleep 1
systemctl is-active voice-expenses
curl -fsS -o /dev/null -w "local http %{http_code}\n" "http://127.0.0.1:$PORT/"
REMOTE
