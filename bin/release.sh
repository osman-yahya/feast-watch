#!/usr/bin/env bash
# Build the mother and every agent binary locally, with the version compiled in.
#
#   bin/release.sh              # version from `git describe`
#   bin/release.sh v1.3.0       # explicit version
#
# This is a developer convenience, not the release path. Releases are built and
# published by .github/workflows/release.yml on a tag push: agents download
# their binaries from the GitHub release, and the mother neither stores nor
# serves them, so there is nothing to stage on a server any more.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT_DIR="${OUT_DIR:-bin/build}"
LDFLAGS="-s -w -X github.com/osman-yahya/feast-watch/shared/version.Version=${VERSION}"

# Must match shared/release.Platforms and the release workflow's matrix.
PLATFORMS=(
  linux-amd64
  linux-arm64
  windows-amd64
  darwin-arm64
)

if [ "$VERSION" = "dev" ] || [[ "$VERSION" == *-dirty ]]; then
  echo "!! version is '$VERSION' — tag the commit before a real release" >&2
fi

mkdir -p "$OUT_DIR"

# checksum writes the .sha256 the agent verifies before replacing itself.
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
go build -ldflags "$LDFLAGS" -o "$OUT_DIR/feast-watch" ./mother/cmd/feast-watch

for platform in "${PLATFORMS[@]}"; do
  goos="${platform%-*}"
  goarch="${platform#*-}"

  # Named exactly as the release asset, so a locally built binary can be
  # uploaded to a release by hand if CI is unavailable.
  out="$OUT_DIR/feast-watch-agent-$platform"
  echo "-> building agent $platform"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -ldflags "$LDFLAGS" -o "$out" ./agent/cmd/feast-watch-agent
  checksum "$out"
done

echo
echo "built in $OUT_DIR:"
ls -1 "$OUT_DIR" | sed 's/^/  /'
