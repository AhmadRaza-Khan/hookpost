#!/usr/bin/env bash
# Scripted demo for the README GIF: events are sent, the receiver logs them,
# the receiver is killed, deliveries fail and retry, the receiver comes back
# and everything is delivered.
#
#   export DATABASE_URL=postgres://postgres:postgres@localhost:5432/hookpost?sslmode=disable
#   ./docs/demo.sh            # or record it: vhs docs/demo.tape
set -euo pipefail
cd "$(dirname "$0")/.."

: "${DATABASE_URL:?set DATABASE_URL to a scratch Postgres database}"
PORT=${PORT:-9000}
tmp=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$tmp"' EXIT

go build -o "$tmp/sender" ./examples/sender
go build -o "$tmp/receiver" ./examples/receiver

say() { printf '\n\033[1;36m### %s\033[0m\n' "$*"; }

say "sender: one order.created per second, signed, retried on failure"
RECEIVER_URL="http://127.0.0.1:$PORT/hooks" "$tmp/sender" > "$tmp/sender.log" 2>&1 &
sleep 1.5
secret=$(grep -o 'whsec_[A-Za-z0-9+/=]*' "$tmp/sender.log" | head -1)
tail -n +1 -f "$tmp/sender.log" \
  | grep --line-buffered -E 'sent|status=' \
  | sed -u -E 's/^[0-9/]+ [0-9:]+ //; s/err=Post "[^"]*": .*connection refused/err=connection refused/; s/^/[sender]   /' &

say "receiver up: verifies every signature"
WEBHOOK_SECRET=$secret ADDR=":$PORT" "$tmp/receiver" 2>&1 \
  | sed -u -E 's/^[0-9\/]+ [0-9:]+ //; s/^/[receiver] /' &
recv=$!
sleep 4

say "receiver down: deliveries fail and are retried"
kill "$recv"; pkill -f "$tmp/receiver" || true
sleep 5

say "receiver back: the backlog drains, same webhook-id on each retry"
WEBHOOK_SECRET=$secret ADDR=":$PORT" "$tmp/receiver" 2>&1 \
  | sed -u -E 's/^[0-9\/]+ [0-9:]+ //; s/^/[receiver] /' &
sleep 6

say "done"
