#!/usr/bin/env bash
# Local smoke test: builds the mother and an agent, runs both on this machine,
# and asserts the behaviour of the current round end to end.
#
#   e2e/local_smoke.sh
#
# No Docker and no systemd — it runs the two binaries directly against a
# throwaway database in a temp directory, which makes it usable on a laptop
# where e2e/e2e_test.sh (docker compose) cannot run. Everything it creates is
# removed on exit, including on failure.
#
# What it proves, in order:
#   1  the mother serves plain HTTP and hands out a plain-HTTP install command
#   2  a push lands in the rollup tiers and the raw samples table does not exist
#   3  the chart reads back what was pushed
#   4  groups: create, duplicate-name conflict, membership, filter, bulk clear
#   5  rollout targets are validated against published GitHub releases
#   6  a settings payload missing a retention key is refused
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${PORT:-18443}"
BASE="http://127.0.0.1:$PORT"
KEY="X-API-Key: smoke-key"
WORK="$(mktemp -d)"
MOTHER_PID=""
AGENT_PID=""

pass() { echo "  ok   $*"; }
fail() { echo "  FAIL $*" >&2; exit 1; }
step() { echo; echo "== $*"; }

cleanup() {
  # `wait` after the kill so the shell reaps each job quietly; without it it
  # prints its own "Terminated" line after the summary, which reads like a
  # failure at the end of a passing run.
  for pid in "$AGENT_PID" "$MOTHER_PID"; do
    [ -n "$pid" ] || continue
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

# api GETs a path and prints the unwrapped `data`.
api() {
  curl -sf -H "$KEY" "$BASE$1"
}

# status_of prints the HTTP status for a request, so a rejection can be
# asserted without the body.
status_of() {
  local method="$1" path="$2" body="${3:-}"
  curl -s -o /dev/null -w '%{http_code}' -H "$KEY" -X "$method" "$BASE$path" ${body:+-d "$body"}
}

# error_of prints the `error` field of a rejected request.
error_of() {
  local method="$1" path="$2" body="${3:-}"
  curl -s -H "$KEY" -X "$method" "$BASE$path" ${body:+-d "$body"} |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["error"])'
}

build() {
  step "building"
  go build -o "$WORK/feast-watch" ./mother/cmd/feast-watch
  go build -o "$WORK/feast-watch-agent" ./agent/cmd/feast-watch-agent
  pass "mother and agent built"
}

start_mother() {
  step "starting the mother on $BASE"

  # Refuse to start onto an occupied port. Without this the readiness check
  # below is satisfied by whatever is already listening — a mother left behind
  # by an interrupted run, with its own database — and every later assertion
  # then runs against the wrong process while this one has silently failed to
  # bind. The symptom is a bare curl exit code with no clue where it came from.
  if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >&2
    fail "port $PORT is already in use; stop that process or set PORT="
  fi

  FW_DB_PATH="$WORK/mother.db" \
  FW_LISTEN="127.0.0.1:$PORT" \
  FW_PUBLIC_URL="$BASE" \
  FW_API_KEY=smoke-key \
    "$WORK/feast-watch" > "$WORK/mother.log" 2>&1 &
  MOTHER_PID=$!

  for _ in $(seq 1 30); do
    if curl -sf -H "$KEY" "$BASE/api/servers" >/dev/null 2>&1; then
      pass "mother answering"
      return 0
    fi
    sleep 0.5
  done
  cat "$WORK/mother.log" >&2
  fail "mother did not come up"
}

# 1 — plain HTTP end to end.
check_plain_http() {
  step "1. plain HTTP"
  local created command
  created=$(curl -sf -H "$KEY" -X POST "$BASE/api/servers" -d '{"name":"smoke-host"}')
  command=$(echo "$created" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["install_command"])')

  case "$command" in
    *" -k "*|*https://*) fail "install command still carries TLS: $command" ;;
    "curl -sSL http://"*) pass "install command is plain HTTP without -k" ;;
    *) fail "unexpected install command: $command" ;;
  esac

  TOKEN=$(echo "$created" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["server"]["token"])')

  # The served installer must fetch binaries from the release host, not here.
  local script
  script=$(curl -sf "$BASE/install/$TOKEN.sh")
  case "$script" in
    *'$MOTHER_URL/download'*) fail "installer still downloads binaries from the mother" ;;
    *"RELEASE_BASE_URL=https://github.com/"*) pass "installer downloads from GitHub Releases" ;;
    *) fail "installer does not name a release host" ;;
  esac
  case "$script" in
    *"feast-watch-agent-uninstall"*) pass "installer leaves an uninstaller on disk" ;;
    *) fail "installer does not install an uninstaller" ;;
  esac
}

start_agent() {
  step "starting an agent"
  # Drop the push interval to the floor first: the agent learns it from its
  # first response, so several pushes then land inside one minute bucket and
  # the aggregation check below does not depend on where a minute boundary
  # happens to fall.
  curl -sf -o /dev/null -H "$KEY" -X PUT "$BASE/api/settings" \
    -d '{"interval":2,"heartbeat_miss_threshold":3,"retention_1m_days":15,"retention_1h_days":75}'

  printf 'MOTHER_URL=%s\nTOKEN=%s\nSERVER_NAME=smoke-host\n' "$BASE" "$TOKEN" > "$WORK/agent.conf"
  "$WORK/feast-watch-agent" -config "$WORK/agent.conf" > "$WORK/agent.log" 2>&1 &
  AGENT_PID=$!

  for _ in $(seq 1 40); do
    if api /api/servers | grep -q '"status":"online"'; then
      pass "agent online"
      return 0
    fi
    sleep 0.5
  done
  cat "$WORK/agent.log" >&2
  fail "agent never reported online"
}

# 2 — write volume: rollups only, no raw tier.
check_write_volume() {
  step "2. ingest writes rollups, not raw samples"

  # Poll rather than sleep a fixed span: a minute boundary can fall between two
  # pushes and split them into separate buckets, so wait for a bucket that
  # actually accumulated instead of assuming the timing.
  local aggregated=0
  for _ in $(seq 1 40); do
    if python3 -c "import sqlite3,sys; c=[n for (n,) in sqlite3.connect(sys.argv[1]).execute('select cnt from rollup_1m')]; sys.exit(0 if c and max(c)>1 else 1)" "$WORK/mother.db" 2>/dev/null; then
      aggregated=1
      break
    fi
    sleep 1
  done
  [ "$aggregated" = "1" ] || fail "no rollup bucket ever accumulated more than one push"

  python3 - "$WORK/mother.db" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
tables = {r[0] for r in db.execute("select name from sqlite_master where type='table'")}
if "samples" in tables:
    sys.exit("  FAIL raw samples table still exists")
print("  ok   no raw samples table")

rows = db.execute("select metric, cnt from rollup_1m order by metric").fetchall()
if not rows:
    sys.exit("  FAIL nothing was written to rollup_1m")
metrics = {m for m, _ in rows}
print("  ok   %d metric(s) folded into rollup_1m, best bucket cnt=%d"
      % (len(metrics), max(c for _, c in rows)))

if not db.execute("select count(*) from rollup_1h").fetchone()[0]:
    sys.exit("  FAIL rollup_1h was not written on ingest")
print("  ok   rollup_1h written on the same push")

if db.execute("pragma journal_mode").fetchone()[0] != "wal":
    sys.exit("  FAIL journal_mode is not wal")
print("  ok   journal_mode=wal")
PY
}

# 3 — the chart reads back what was pushed.
check_chart() {
  step "3. chart reads the rollups"
  local now points
  now=$(date +%s)
  points=$(api "/api/chart?server_id=1&metric=cpu.usage&from=$((now - 3600))&to=$now&interval=60" |
    python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))')
  [ "$points" -ge 1 ] || fail "chart returned $points points"
  pass "chart returned $points point(s)"
}

# 4 — groups.
check_groups() {
  step "4. server groups"
  local code
  code=$(status_of POST /api/groups '{"name":"Veritabanı Sunucuları"}')
  [ "$code" = "200" ] || fail "group create returned $code"
  pass "group created (unicode name accepted)"

  code=$(status_of POST /api/groups '{"name":"Veritabanı Sunucuları"}')
  [ "$code" = "409" ] || fail "duplicate group name returned $code, want 409"
  pass "duplicate name refused with 409"

  code=$(status_of PUT /api/groups/1/servers '{"server_ids":[1]}')
  [ "$code" = "200" ] || fail "setting members returned $code"

  local members
  members=$(api "/api/servers?group_id=1" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))')
  [ "$members" = "1" ] || fail "group filter returned $members servers, want 1"
  pass "group filter narrows the fleet list"

  api /api/servers | grep -q '"groups":\[{' || fail "server rows do not carry their groups"
  pass "server rows carry their groups"

  # Clearing must reach every member regardless of platform.
  local applied
  applied=$(curl -sf -H "$KEY" -X PUT "$BASE/api/groups/1/version" -d '{"version":""}' |
    python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]["applied"]))')
  [ "$applied" = "1" ] || fail "group clear applied to $applied servers"
  pass "bulk clear reaches every member"
}

# 5 — rollout targets are checked against published releases.
check_rollout_validation() {
  step "5. rollout targets are validated"
  local msg
  msg=$(error_of PUT /api/servers/1/version '{"version":"v9.9.9"}')
  case "$msg" in
    *"no published release"*) pass "unpublished version refused: $msg" ;;
    *) fail "unexpected rejection: $msg" ;;
  esac

  msg=$(error_of PUT /api/servers/1/version '{"version":"latest"}')
  case "$msg" in
    *"moving alias"*) pass "the latest alias is refused" ;;
    *) fail "unexpected rejection: $msg" ;;
  esac

  # The index itself must have been reachable, whatever it contains.
  api /api/version | grep -q '"checked_at"' || fail "version endpoint reports no check time"
  pass "release index reports its freshness"
}

# 6 — the settings guard that used to wipe a retention tier.
check_settings_guard() {
  step "6. settings payloads must be complete"
  local msg code
  msg=$(error_of PUT /api/settings '{"interval":10,"heartbeat_miss_threshold":3,"retention_1h_days":75}')
  case "$msg" in
    *retention_1m_days*required*) pass "partial payload refused: $msg" ;;
    *) fail "a partial settings payload was accepted (msg: $msg)" ;;
  esac

  code=$(status_of PUT /api/settings '{"interval":10,"heartbeat_miss_threshold":3,"retention_1m_days":15,"retention_1h_days":75}')
  [ "$code" = "200" ] || fail "a complete payload returned $code"
  pass "complete payload accepted"
}

main() {
  build
  start_mother
  check_plain_http
  start_agent
  check_write_volume
  check_chart
  check_groups
  check_rollout_validation
  check_settings_guard

  echo
  echo "all checks passed"
}

main "$@"
