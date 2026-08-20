#!/usr/bin/env bash
# The promote helper: the root half of the mother's self-update.
#
# The mother runs sandboxed and unprivileged, so it can verify a new binary but
# never install one. It stages the verified file inside its StateDirectory and
# exits; systemd runs this from ExecStartPre with a `+` prefix — as root, and
# outside the sandbox — before ExecStart runs the result.
#
# Runs against a temp tree via FW_ROOT, so it needs neither root nor systemd.
set -euo pipefail

cd "$(dirname "$0")/.."

ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT

pass() { echo "  ok   $1"; }
fail() { echo "  FAIL $1" >&2; exit 1; }
step() { echo; echo "== $1"; }

BIN="$ROOT/usr/local/bin/feast-watch"
STAGED="$ROOT/var/lib/feast-watch/update/feast-watch.new"
mkdir -p "$(dirname "$BIN")" "$(dirname "$STAGED")"

step "1. nothing staged is a no-op, not a failure"
printf 'old' > "$BIN"
FW_ROOT="$ROOT" bash deploy/feast-watch-mother-promote > /dev/null ||
  fail "the helper failed with nothing staged — it runs on EVERY start, including ordinary ones"
[ "$(cat "$BIN")" = "old" ] || fail "the binary changed with nothing staged"
pass "no-op with nothing staged"

step "2. a staged binary is installed, and the old one kept"
printf 'new' > "$STAGED"
chmod 0755 "$STAGED"
FW_ROOT="$ROOT" bash deploy/feast-watch-mother-promote > /dev/null || fail "promote failed"
[ "$(cat "$BIN")" = "new" ] || fail "the staged binary was not installed"
[ "$(cat "$BIN.bak")" = "old" ] || fail "the previous binary was not kept as .bak — nothing to roll back to"
[ -x "$BIN" ] || fail "the installed binary is not executable"
[ ! -e "$STAGED" ] || fail "the staged file was left behind — it would be promoted again on every start"
pass "staged binary promoted, previous kept as .bak"

step "3. running again with nothing staged changes nothing"
FW_ROOT="$ROOT" bash deploy/feast-watch-mother-promote > /dev/null || fail "second run failed"
[ "$(cat "$BIN")" = "new" ] || fail "a second run altered the binary"
pass "idempotent"

step "4. an empty staged file is refused and cleared"
: > "$STAGED"
chmod 0755 "$STAGED"
FW_ROOT="$ROOT" bash deploy/feast-watch-mother-promote > /dev/null 2>&1 ||
  fail "the helper must never fail the boot"
[ "$(cat "$BIN")" = "new" ] || fail "an empty staged file replaced the binary"
[ ! -e "$STAGED" ] || fail "the refused staged file was left to be retried forever"
pass "empty staged file refused and cleared"

step "5. the staged path is the one the mother writes"
grep -q 'feast-watch.new' mother/selfupdate/updater.go ||
  fail "mother/selfupdate no longer names feast-watch.new; the two halves have drifted"
grep -q 'update/feast-watch.new' deploy/feast-watch-mother-promote ||
  fail "the helper no longer looks in update/feast-watch.new"
pass "both halves agree on the staged path"

step "6. the unit runs the helper as root before starting"
grep -q '^ExecStartPre=+/usr/local/sbin/feast-watch-mother-promote$' deploy/feast-watch-mother.service ||
  fail "the unit does not run the promote helper with a + prefix; a staged binary could never be installed"
pass "ExecStartPre runs it outside the sandbox"

echo
echo "all checks passed"
