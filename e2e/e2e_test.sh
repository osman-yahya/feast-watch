#!/usr/bin/env bash
# End-to-end: mother + 2 agents via compose; asserts push → status → chart.
set -euo pipefail
cd "$(dirname "$0")/.."

API="http://localhost:8443"
KEY="X-API-Key: dev-api-key"

cleanup() { docker compose down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker compose up -d --build mother
sleep 2

echo "-> add two servers via admin API"
TOKEN1=$(curl -sf -H "$KEY" -X POST "$API/api/servers" -d '{"name":"e2e-1"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["server"]["token"])')
TOKEN2=$(curl -sf -H "$KEY" -X POST "$API/api/servers" -d '{"name":"e2e-2"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["server"]["token"])')

mkdir -p e2e
printf 'MOTHER_URL=http://mother:8443\nTOKEN=%s\nSERVER_NAME=e2e-1\n' "$TOKEN1" > e2e/agent-1.conf
printf 'MOTHER_URL=http://mother:8443\nTOKEN=%s\nSERVER_NAME=e2e-2\n' "$TOKEN2" > e2e/agent-2.conf

docker compose up -d --build agent-1 agent-2
echo "-> waiting for pushes"
sleep 15

echo "-> both servers must be online"
STATUSES=$(curl -sf -H "$KEY" "$API/api/servers" | python3 -c 'import sys,json;print(",".join(sorted(s["status"] for s in json.load(sys.stdin)["data"])))')
[ "$STATUSES" = "online,online" ] || { echo "FAIL: statuses=$STATUSES"; exit 1; }

echo "-> chart must return rollup points"
SID=$(curl -sf -H "$KEY" "$API/api/servers" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"][0]["id"])')
# Ingest folds each push into both rollup tiers as it arrives, so there is no
# background job to wait for — only enough pushes to fill a minute bucket.
sleep 15
NOW=$(date +%s)
POINTS=$(curl -sf -H "$KEY" "$API/api/chart?server_id=$SID&metric=cpu.usage&from=$((NOW-600))&to=$NOW&interval=60" \
  | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["data"]))')
[ "$POINTS" -ge 1 ] || { echo "FAIL: no chart points"; exit 1; }

echo "E2E PASS"
