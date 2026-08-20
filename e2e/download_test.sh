#!/usr/bin/env bash
# mother-install.sh --download: the path that removes every build dependency
# from a host being deployed.
#
# The mother is a statically linked pure-Go binary that shells out to nothing,
# so a host that can run it needs no toolchain — but until this existed the
# INSTALLER did, because the only way to get a binary was to build one from a
# checkout. This exercises the fetch-and-verify half against a local release
# tree, so no network and no root are needed.
set -euo pipefail

cd "$(dirname "$0")/.."

ROOT=$(mktemp -d)
SERVE=$(mktemp -d)
PORT=${PORT:-18651}
trap 'rm -rf "$ROOT" "$SERVE"; [ -n "${HTTP_PID:-}" ] && kill "$HTTP_PID" 2>/dev/null; true' EXIT

pass() { echo "  ok   $1"; }
fail() { echo "  FAIL $1" >&2; exit 1; }
step() { echo; echo "== $1"; }

sha256_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$1" | cut -d' ' -f1; }

# A local tree shaped like GitHub's release URLs, so the installer's own URL
# construction is what is under test rather than a stubbed-out version of it.
mkdir -p "$SERVE/releases/latest/download" "$SERVE/releases/download/v1.4.0"
printf 'MOTHER-BINARY' > "$SERVE/releases/latest/download/feast-watch-mother-linux-amd64"
sha256_of "$SERVE/releases/latest/download/feast-watch-mother-linux-amd64" \
  > "$SERVE/releases/latest/download/feast-watch-mother-linux-amd64.sha256"
printf 'MOTHER-v1.4.0' > "$SERVE/releases/download/v1.4.0/feast-watch-mother-linux-amd64"
sha256_of "$SERVE/releases/download/v1.4.0/feast-watch-mother-linux-amd64" \
  > "$SERVE/releases/download/v1.4.0/feast-watch-mother-linux-amd64.sha256"

# A build whose checksum does not describe it: the case the verification exists
# for, and the one that must never reach disk.
printf 'TAMPERED' > "$SERVE/releases/latest/download/feast-watch-mother-linux-arm64"
printf 'deadbeef' > "$SERVE/releases/latest/download/feast-watch-mother-linux-arm64.sha256"

# A build published without its checksum at all.
printf 'UNVERIFIABLE' > "$SERVE/releases/latest/download/feast-watch-agent-linux-amd64"

# --directory rather than a subshell that cd's: with a subshell, $! is the
# subshell's pid and the trap can leave the server running against a deleted
# temp tree — which the next run then reaches, and reads as a 404 from its own
# fixture.
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$SERVE" >/dev/null 2>&1 &
HTTP_PID=$!

ready=0
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$PORT/releases/latest/download/feast-watch-mother-linux-amd64" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.1
done
# Fail here rather than letting every assertion below fail as a 404: a fixture
# that never came up is not the installer being wrong.
[ "$ready" = 1 ] || fail "the local release host never came up on port $PORT"

BASE="http://127.0.0.1:$PORT"

# The installer is sourced, not run: main() needs root and systemd, while the
# download half needs neither. Sourcing runs the definitions and stops there.
run_fetch() {
  FW_ROOT="$ROOT" RELEASE_BASE_URL="$BASE" bash -c '
    set -euo pipefail
    # shellcheck disable=SC1091
    source deploy/mother-install.sh
    fetch_release_binary "$1" "$2" "$3"
  ' _ "$@"
}

step "1. the published build is downloaded and installed"
DEST="$ROOT/feast-watch"
run_fetch feast-watch-mother-linux-amd64 "$DEST" latest || fail "download failed"
[ "$(cat "$DEST")" = "MOTHER-BINARY" ] || fail "the wrong bytes were installed"
[ -x "$DEST" ] || fail "the installed binary is not executable"
pass "latest downloaded, verified and installed"

step "2. a pinned version is fetched from its own tag, not from latest"
run_fetch feast-watch-mother-linux-amd64 "$DEST" v1.4.0 || fail "pinned download failed"
[ "$(cat "$DEST")" = "MOTHER-v1.4.0" ] || fail "--download=v1.4.0 did not fetch the tagged build"
pass "a pinned version comes from its tag"

step "3. bytes that do not match their checksum are refused"
BAD="$ROOT/bad"
if run_fetch feast-watch-mother-linux-arm64 "$BAD" latest 2>/dev/null; then
  fail "a binary whose checksum did not match was installed"
fi
[ ! -e "$BAD" ] || fail "the refused binary reached its destination anyway"
pass "checksum mismatch refused, nothing written"

step "4. a build published with no checksum is refused, not trusted"
if run_fetch feast-watch-agent-linux-amd64 "$BAD" latest 2>/dev/null; then
  fail "a build with no published checksum was installed"
fi
[ ! -e "$BAD" ] || fail "an unverifiable binary reached its destination"
pass "missing checksum refused"

step "5. a missing asset is an error, not an error page installed as a binary"
# Without curl --fail this is the dangerous one: curl exits 0, writes the 404
# body, and systemd respawns against it every five seconds.
if run_fetch feast-watch-mother-linux-amd64 "$BAD" v9.9.9 2>/dev/null; then
  fail "a 404 was installed as the mother binary"
fi
[ ! -e "$BAD" ] || fail "a 404 body was written to the destination"
pass "an unpublished version fails instead of installing a 404 page"

step "6. an architecture with no published mother build is named, not guessed"
out=$(FW_ROOT="$ROOT" bash -c '
  set -euo pipefail
  # shellcheck disable=SC1091
  source deploy/mother-install.sh
  uname() { echo "ppc64le"; }
  export -f uname
  release_platform
' 2>&1) && fail "an unsupported architecture was accepted: $out"
case "$out" in
  *ppc64le*) pass "the unsupported architecture is named in the error" ;;
  *) fail "the error does not say which architecture failed: $out" ;;
esac

echo
echo "all checks passed"
