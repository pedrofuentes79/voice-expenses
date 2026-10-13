#!/usr/bin/env bash
# Run on the host, inside the checkout: reset to origin/main, fetch the CI-built binary for
# exactly that commit, swap it in and restart the service. Data (data/, .env) is gitignored
# and untouched by the reset.
set -euo pipefail
cd "$(dirname "$0")/.."

RELEASE=https://github.com/pedrofuentes79/voice-expenses/releases/download/main-latest
ASSET=voice-expenses-linux-arm64
SERVICE=${SERVICE:-voice-expenses}
WAIT_SECONDS=${WAIT_SECONDS:-600}

git fetch -q origin main
git reset -q --hard origin/main
want=$(git rev-parse HEAD)
echo "main is at ${want:0:7}"

# CI publishes a few minutes after the push; wait for the build of this exact commit.
deadline=$((SECONDS + WAIT_SECONDS))
until [[ "$(curl -fsSL "$RELEASE/commit" 2>/dev/null)" == "$want" ]]; do
    if (( SECONDS >= deadline )); then
        echo "no CI build for ${want:0:7} after ${WAIT_SECONDS}s; check the build workflow" >&2
        exit 1
    fi
    echo "waiting for the CI build of ${want:0:7}..."
    sleep 15
done

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsSL -o "$tmp" "$RELEASE/$ASSET"
expected=$(curl -fsSL "$RELEASE/$ASSET.sha256")
echo "$expected  $tmp" | sha256sum -c --quiet
install -m 755 "$tmp" voice-expenses
echo "$want" > .deployed-commit

sudo systemctl restart "$SERVICE"
sleep 1
if systemctl is-active --quiet "$SERVICE"; then
    echo "$SERVICE running ${want:0:7}"
else
    journalctl -u "$SERVICE" -n 20 --no-pager >&2
    exit 1
fi
