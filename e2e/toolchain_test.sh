#!/usr/bin/env bash
# mother-install.sh and the Go toolchain: the compiler the mother needs to make
# binaries for its fleet at all.
#
# The mother compiles every agent on the fleet, so a mother host without a
# working `go` is a fleet that can never be updated — and the failure would
# arrive at the first `feast-watch build`, long after everyone believed the
# install had succeeded. The installer therefore provides one, which makes the
# download-and-verify path here as load-bearing as the one in download_test.sh.
#
# Exercised against a local tree and a temp root, so no network and no root.
set -euo pipefail

cd "$(dirname "$0")/.."

ROOT=$(mktemp -d)
SERVE=$(mktemp -d)
BIN=$(mktemp -d)
PORT=${PORT:-18652}
trap 'rm -rf "$ROOT" "$SERVE" "$BIN"; [ -n "${HTTP_PID:-}" ] && kill "$HTTP_PID" 2>/dev/null; true' EXIT

pass() { echo "  ok   $1"; }
fail() { echo "  FAIL $1" >&2; exit 1; }
step() { echo; echo "== $1"; }

sha256_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$1" | cut -d' ' -f1; }

# A tarball shaped like go.dev's: one top-level `go/` directory with a runnable
# binary inside it, which is what the installer extracts and links to.
mkdir -p "$BIN/go/bin"
cat > "$BIN/go/bin/go" <<'EOF'
#!/usr/bin/env bash
echo "go version go1.26.7 linux/amd64"
EOF
chmod 0755 "$BIN/go/bin/go"
tar -C "$BIN" -czf "$SERVE/go1.26.7.linux-amd64.tar.gz" go
FIXTURE_SUM=$(sha256_of "$SERVE/go1.26.7.linux-amd64.tar.gz")

# A tarball whose published checksum does not describe it: the case the
# verification exists for, on the one download that becomes the compiler every
# binary on the fleet is built by.
cp "$SERVE/go1.26.7.linux-amd64.tar.gz" "$SERVE/go1.26.7.linux-arm64.tar.gz"

python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$SERVE" >/dev/null 2>&1 &
HTTP_PID=$!

ready=0
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$PORT/go1.26.7.linux-amd64.tar.gz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.1
done
[ "$ready" = 1 ] || fail "the local download host never came up on port $PORT"

BASE="http://127.0.0.1:$PORT"

# The installer is sourced, not run: main() needs root and systemd, while the
# toolchain half needs neither. `go_sha256` is redefined afterwards so the happy
# path can be exercised against a fixture without loosening the pin that
# protects the real download.
#
# PATH is replaced rather than extended. These cases are about what the
# installer does when the host has no Go, or an old one — and the machine
# running the test almost certainly has a perfectly good Go on PATH, which
# would answer every question before the code under test was reached.
BASE_PATH=/usr/bin:/bin:/usr/sbin:/sbin
run_ensure_go() {
  FW_ROOT="$ROOT" GO_DL_BASE="$BASE" SUM="$1" PRESENT="${2:-}" PATH="$BASE_PATH" bash -c '
    set -euo pipefail
    # shellcheck disable=SC1091
    source deploy/mother-install.sh
    go_sha256() { echo "$SUM"; }
    [ -n "$PRESENT" ] && PATH="$PRESENT:$PATH"
    ensure_go linux-amd64
  '
}

step "1. a host with no Go gets the pinned toolchain, verified"
out=$(run_ensure_go "$FIXTURE_SUM") || fail "ensure_go failed: $out"
[ -x "$ROOT/usr/local/go/bin/go" ] || fail "the toolchain was not extracted"
[ -L "$ROOT/usr/local/bin/go" ] || fail "no symlink on the default PATH — the service account would never find it"
[ "$("$ROOT/usr/local/bin/go" version)" = "go version go1.26.7 linux/amd64" ] ||
  fail "the symlink does not resolve to the installed toolchain"
pass "downloaded, verified, extracted and linked"

step "2. a tarball that does not match its pinned checksum is refused"
rm -rf "${ROOT:?}/usr"
if run_ensure_go "deadbeef" 2>/dev/null; then
  fail "a toolchain whose checksum did not match was installed"
fi
[ ! -e "$ROOT/usr/local/go" ] || fail "the refused toolchain reached the filesystem anyway"
pass "checksum mismatch refused, nothing extracted"

step "3. a Go already on the host is used rather than downloaded over"
rm -rf "${ROOT:?}/usr"
mkdir -p "$BIN/existing"
cat > "$BIN/existing/go" <<'EOF'
#!/usr/bin/env bash
echo "go version go1.28.0 linux/amd64"
EOF
chmod 0755 "$BIN/existing/go"
out=$(run_ensure_go "$FIXTURE_SUM" "$BIN/existing") || fail "ensure_go failed: $out"
case "$out" in
  *"already on this host"*) pass "the existing toolchain is kept" ;;
  *) fail "an existing Go was not detected: $out" ;;
esac
[ ! -e "$ROOT/usr/local/go" ] || fail "a second toolchain was installed beside the host's own"

step "4. a Go too old to build this tree is replaced, not trusted"
rm -rf "${ROOT:?}/usr"
mkdir -p "$BIN/ancient"
cat > "$BIN/ancient/go" <<'EOF'
#!/usr/bin/env bash
echo "go version go1.19.13 linux/amd64"
EOF
chmod 0755 "$BIN/ancient/go"
run_ensure_go "$FIXTURE_SUM" "$BIN/ancient" >/dev/null || fail "ensure_go failed against an old toolchain"
[ -x "$ROOT/usr/local/go/bin/go" ] ||
  fail "a Go older than the tree's own go.mod line was accepted; the build would fail with syntax errors"
pass "an unusable toolchain is replaced"

step "5. an architecture with no pinned toolchain is named, not guessed"
out=$(FW_ROOT="$ROOT" PATH="$BASE_PATH" bash -c '
  set -euo pipefail
  # shellcheck disable=SC1091
  source deploy/mother-install.sh
  install_go linux-riscv64
' 2>&1) && fail "an unpinned architecture was accepted: $out"
case "$out" in
  *riscv64*|*"--skip-go"*) pass "the unsupported architecture is named, with a way out" ;;
  *) fail "the error does not say what failed: $out" ;;
esac

step "6. a toolchain this installer put here is removed with --purge; one it found is not"
rm -rf "${ROOT:?}/usr"; mkdir -p "$ROOT/etc/feast-watch" "$ROOT/usr/local/go/bin" "$ROOT/usr/local/bin"
: > "$ROOT/usr/local/go/bin/go"
ln -sf "$ROOT/usr/local/go/bin/go" "$ROOT/usr/local/bin/go"
printf 'bin=%s/usr/local/bin/feast-watch\ngo=%s/usr/local/go\ngo_link=%s/usr/local/bin/go\n' \
  "$ROOT" "$ROOT" "$ROOT" > "$ROOT/etc/feast-watch/mother-manifest"
FW_ROOT="$ROOT" bash deploy/mother-uninstall.sh --purge >/dev/null 2>&1
[ ! -e "$ROOT/usr/local/go" ] || fail "a toolchain this installer declared was left behind"
[ ! -L "$ROOT/usr/local/bin/go" ] || fail "the dangling symlink was left on PATH"
pass "a declared toolchain is removed"

rm -rf "${ROOT:?}/usr" "${ROOT:?}/etc"
mkdir -p "$ROOT/etc/feast-watch" "$ROOT/usr/local/go/bin"
: > "$ROOT/usr/local/go/bin/go"
printf 'bin=%s/usr/local/bin/feast-watch\n' "$ROOT" > "$ROOT/etc/feast-watch/mother-manifest"
FW_ROOT="$ROOT" bash deploy/mother-uninstall.sh --purge >/dev/null 2>&1
[ -e "$ROOT/usr/local/go" ] || fail "a Go the installer never touched was removed"
pass "a toolchain that was already here survives"

step "7. --build compiles a mother from this checkout with that toolchain"
# The from-scratch path: no published release in the picture, and nothing built
# beforehand. OUT_DIR keeps the artifacts out of the working tree; what is under
# test is that the installer can produce the binary it is about to install.
BUILD_OUT=$(mktemp -d)
trap 'rm -rf "$ROOT" "$SERVE" "$BIN" "$BUILD_OUT"; [ -n "${HTTP_PID:-}" ] && kill "$HTTP_PID" 2>/dev/null; true' EXIT
OUT_DIR="$BUILD_OUT" bash -c '
  set -euo pipefail
  # shellcheck disable=SC1091
  source deploy/mother-install.sh
  build_here 0
' >/dev/null 2>&1 || fail "--build could not compile the mother from this checkout"
[ -x "$BUILD_OUT/feast-watch" ] || fail "the build produced no mother binary to install"
pass "a mother is compiled from source, no release required"

echo
echo "all checks passed"
