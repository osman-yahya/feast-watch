#!/usr/bin/env bash
# Build the mother and every agent binary with the version compiled in, and
# stage the agent builds where the mother serves them from.
#
#   bin/release.sh              # version from `git describe`
#   bin/release.sh v1.3.0       # explicit version
#   OUT_DIR=/var/lib/feast-watch/downloads bin/release.sh
#
# Without this, shared/version.Version stays "dev" in every build: the panel
# shows "dev" for every agent, and "update this server to v1.3.0" can never be
# satisfied because no agent ever reports being on v1.3.0.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT_DIR="${OUT_DIR:-downloads}"
BIN_DIR="${BIN_DIR:-bin}"
LDFLAGS="-s -w -X github.com/osman-yahya/feast-watch/shared/version.Version=${VERSION}"

# Platforms agents run on. Must stay in sync with knownPlatforms in
# mother/api/versions.go, which is what decides whether a build is offered as
# a rollout target in the panel.
PLATFORMS=(
  linux-amd64
  linux-arm64
  windows-amd64
  darwin-arm64
)

if [ "$VERSION" = "dev" ] || [[ "$VERSION" == *-dirty ]]; then
  echo "!! version is '$VERSION' — tag the commit before a real release" >&2
fi

mkdir -p "$OUT_DIR" "$BIN_DIR"

# checksum writes the .sha256 the agent verifies before replacing itself. A
# build without one is never offered by the mother, so this is not optional.
checksum() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | cut -d' ' -f1 > "$file.sha256"
  else
    shasum -a 256 "$file" | cut -d' ' -f1 > "$file.sha256"
  fi
}

echo "-> version $VERSION"

echo "-> building mother"
go build -ldflags "$LDFLAGS" -o "$BIN_DIR/feast-watch" ./mother/cmd/feast-watch

for platform in "${PLATFORMS[@]}"; do
  goos="${platform%-*}"
  goarch="${platform#*-}"

  # No .exe suffix even for windows: the agent replaces its own executable at
  # whatever path it already runs from, and both the download URL and the
  # mother's build listing key on the bare "<version>-<goos>-<goarch>" name.
  out="$OUT_DIR/feast-watch-agent-$VERSION-$platform"
  echo "-> building agent $platform"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -ldflags "$LDFLAGS" -o "$out" ./agent/cmd/feast-watch-agent
  checksum "$out"

  # The install script fetches the moving "latest" pointer, so every release
  # has to move it or new installs keep landing on the previous version.
  latest="$OUT_DIR/feast-watch-agent-latest-$platform"
  cp "$out" "$latest"
  checksum "$latest"

  # Compatibility alias for agents installed before builds carried a GOOS:
  # they request "<version>-<arch>" and cannot be updated without it. Linux
  # only — it is the platform those agents were installed on. Drop this once
  # no agent reports a version older than the first platform-explicit build.
  if [ "$goos" = "linux" ]; then
    legacy="$OUT_DIR/feast-watch-agent-$VERSION-$goarch"
    cp "$out" "$legacy"
    checksum "$legacy"
  fi
done

echo
echo "staged in $OUT_DIR:"
ls -1 "$OUT_DIR" | grep -- "-$VERSION-\|-latest-" | sed 's/^/  /' || true
echo
echo "mother binary: $BIN_DIR/feast-watch"
