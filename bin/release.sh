#!/usr/bin/env bash
# Build the mother and every agent binary locally, with the version compiled in.
#
#   bin/release.sh                     # version from `git describe`
#   bin/release.sh v1.3.0              # explicit version
#   bin/release.sh --mother-only       # just the mother, for a host being deployed
#
# This is a developer convenience, not the release path. Releases are built and
# published by .github/workflows/release.yml on a tag push: agents download
# their binaries from the GitHub release, and the mother neither stores nor
# serves them, so there is nothing to stage on a server any more.
#
# --mother-only exists because deploying a mother is the one case where the
# agent builds are pure waste: the mother host compiles from source, and every
# agent on the fleet — including the one beside the mother — downloads its own
# binary from the release. Four cross-compiles and eight files, none of them
# ever read. The flag lives here rather than in a second script so the ldflags
# string, which is what actually has to stay right, has one home.
set -euo pipefail

cd "$(dirname "$0")/.."

MOTHER_ONLY=0
VERSION=""
for arg in "$@"; do
  case "$arg" in
    --mother-only) MOTHER_ONLY=1 ;;
    -h|--help) sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown option: $arg" >&2; exit 2 ;;
    *) VERSION="$arg" ;;
  esac
done

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT_DIR="${OUT_DIR:-bin/build}"
LDFLAGS="-s -w -X github.com/osman-yahya/feast-watch/shared/version.Version=${VERSION}"

# Must match shared/release.Platforms and the release workflow's matrix.
PLATFORMS=(
  linux-amd64
  linux-arm64
  windows-amd64
  darwin-arm64
)

# Must match shared/release.MotherPlatforms and the release workflow's
# build-mother matrix.
MOTHER_PLATFORMS=(
  linux-amd64
  linux-arm64
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

if [ "$MOTHER_ONLY" -eq 1 ]; then
  echo
  echo "built in $OUT_DIR:"
  echo "  feast-watch"
  echo
  echo "next: sudo deploy/mother-install.sh $OUT_DIR/feast-watch"
  exit 0
fi

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

for platform in "${MOTHER_PLATFORMS[@]}"; do
  goos="${platform%-*}"
  goarch="${platform#*-}"

  # Named exactly as the release asset, so a locally built mother can be
  # uploaded to a release by hand if CI is unavailable. Distinct from the
  # host-native build above, which is what --mother-only produces for a host
  # being deployed — a different job from publishing.
  out="$OUT_DIR/feast-watch-mother-$platform"
  echo "-> building mother $platform"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -ldflags "$LDFLAGS" -o "$out" ./mother/cmd/feast-watch
  checksum "$out"
done

# A tag that only exists on this laptop publishes nothing: the release workflow
# fires on `push: tags`, so an unpushed tag means no run, no release, no assets
# and no version in the panel. That is exactly how v1.0.1 was "cut" and then
# never appeared — the binaries were built here, with the version linked in,
# and everything looked like a release had happened.
warn_if_tag_is_local_only() {
  case "$VERSION" in v*) ;; *) return 0 ;; esac
  git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null || return 0
  if ! git ls-remote --tags origin "refs/tags/$VERSION" 2>/dev/null | grep -q .; then
    echo
    echo "!! tag $VERSION is not on origin — nothing is published yet." >&2
    echo "   run: git push origin $VERSION" >&2
  fi
}

echo
echo "built in $OUT_DIR:"
# shellcheck disable=SC2012  # our own output directory, and every name in
# it was chosen a few lines above — find(1) buys nothing and -printf is not
# portable to the macOS this script also runs on.
ls -1 "$OUT_DIR" | sed 's/^/  /'
warn_if_tag_is_local_only
