#!/usr/bin/env bash
# Asserts that a mother and an agent installed on the SAME host can each be
# removed without destroying the other's configuration.
#
#   e2e/colocation_test.sh
#
# The design expects this deployment — the mother monitors its own host — and
# the two share /etc/feast-watch. Before this test, either uninstaller's
# --purge removed that whole directory: removing the agent took the mother's
# API key with it, and removing the mother took the agent's token, which no
# endpoint reissues.
#
# Runs against a temp tree via FW_ROOT, so it needs neither root nor systemd.
set -euo pipefail

cd "$(dirname "$0")/.."

ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

pass() { echo "  ok   $*"; }
fail() { echo "  FAIL $*" >&2; exit 1; }
step() { echo; echo "== $*"; }

# lay_out_host builds the tree both installers would have produced.
lay_out_host() {
  mkdir -p "$ROOT/usr/local/bin" "$ROOT/usr/local/sbin" \
           "$ROOT/etc/feast-watch" "$ROOT/etc/systemd/system" "$ROOT/var/lib/feast-watch"

  : > "$ROOT/usr/local/bin/feast-watch"
  : > "$ROOT/usr/local/bin/feast-watch-agent"
  : > "$ROOT/usr/local/sbin/feast-watch-agent-uninstall"
  : > "$ROOT/etc/systemd/system/feast-watch-mother.service"
  : > "$ROOT/etc/systemd/system/feast-watch-agent.service"
  : > "$ROOT/var/lib/feast-watch/mother.db"

  printf 'FW_API_KEY=the-api-key\nFW_LISTEN=:8443\n' > "$ROOT/etc/feast-watch/mother.env"
  printf 'MOTHER_URL=http://127.0.0.1:8443\nTOKEN=tk_irreplaceable\n' > "$ROOT/etc/feast-watch/agent.conf"
  : > "$ROOT/etc/feast-watch/mother-manifest"
  : > "$ROOT/etc/feast-watch/install-manifest"
}

exists() { [ -e "$ROOT$1" ]; }

step "1. removing the agent leaves the mother intact"
lay_out_host
FW_ROOT="$ROOT" bash mother/api/uninstall.sh --purge > "$ROOT/agent-out" 2>&1 ||
  { cat "$ROOT/agent-out" >&2; fail "agent uninstaller exited non-zero"; }

exists /etc/feast-watch/agent.conf && fail "the agent's own config survived --purge"
exists /usr/local/bin/feast-watch-agent && fail "the agent binary survived"
exists /etc/systemd/system/feast-watch-agent.service && fail "the agent unit survived"
pass "agent's own files removed"

exists /etc/feast-watch/mother.env || fail "the mother's API key was destroyed by the agent uninstaller"
exists /usr/local/bin/feast-watch || fail "the mother binary was removed by the agent uninstaller"
exists /var/lib/feast-watch/mother.db || fail "the mother's database was removed by the agent uninstaller"
pass "mother untouched"

step "2. removing the mother leaves the agent's token intact"
rm -rf "$ROOT"; ROOT="$(mktemp -d)"; lay_out_host
FW_ROOT="$ROOT" bash deploy/mother-uninstall.sh --purge > "$ROOT/mother-out" 2>&1 ||
  { cat "$ROOT/mother-out" >&2; fail "mother uninstaller exited non-zero"; }

exists /etc/feast-watch/mother.env && fail "the mother's own env file survived --purge"
exists /var/lib/feast-watch && fail "the mother's state directory survived --purge"
pass "mother's own files removed"

exists /etc/feast-watch/agent.conf || fail "the agent's token was destroyed by the mother uninstaller"
grep -q tk_irreplaceable "$ROOT/etc/feast-watch/agent.conf" || fail "the agent's token was rewritten"
exists /usr/local/bin/feast-watch-agent || fail "the agent binary was removed by the mother uninstaller"
pass "agent untouched, token preserved"

step "3. the last one out removes the shared directory"
rm -rf "$ROOT"; ROOT="$(mktemp -d)"; lay_out_host
FW_ROOT="$ROOT" bash deploy/mother-uninstall.sh --purge > /dev/null 2>&1
FW_ROOT="$ROOT" bash mother/api/uninstall.sh --purge > /dev/null 2>&1
exists /etc/feast-watch && fail "/etc/feast-watch survived both uninstallers"
pass "shared directory removed once it was empty"

step "4. each uninstaller is idempotent on an already-clean host"
FW_ROOT="$ROOT" bash deploy/mother-uninstall.sh --purge > /dev/null 2>&1 || fail "mother uninstaller failed on a clean host"
FW_ROOT="$ROOT" bash mother/api/uninstall.sh --purge > /dev/null 2>&1 || fail "agent uninstaller failed on a clean host"
pass "both re-run cleanly"

echo
echo "all checks passed"
