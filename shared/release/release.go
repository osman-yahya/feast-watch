// Package release is the single source of truth for how agent builds are named
// and where they are downloaded from.
//
// It is imported by the agent (to build its own download URL), by the mother
// (to index what a GitHub release offers) and mirrored by the release
// workflow's build matrix. Before this existed the mother's platform list and
// the release script's had already drifted, with a comment saying they must
// agree as the only enforcement.
package release

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// DefaultBaseURL is the public repository agents download from. Public is what
// makes the download token-free; making the repository private would break
// every agent's update path at once.
const DefaultBaseURL = "https://github.com/osman-yahya/feast-watch"

// DefaultAPIBaseURL is where the mother reads the list of releases.
const DefaultAPIBaseURL = "https://api.github.com"

// Repo is the owner/name pair the API path is built from.
const Repo = "osman-yahya/feast-watch"

// assetPrefix is the stem every agent asset shares.
const assetPrefix = "feast-watch-agent-"

// ChecksumSuffix marks the companion file the agent verifies before replacing
// itself. A release without one is unusable and is never offered.
const ChecksumSuffix = ".sha256"

// sha256HexLen is the length of a hex-encoded SHA-256 digest.
const sha256HexLen = 64

// Platform is one GOOS/GOARCH pair a release is built for.
type Platform struct {
	GOOS   string
	GOARCH string
}

func (p Platform) String() string { return p.GOOS + "-" + p.GOARCH }

// Platforms is the build matrix. The release workflow builds exactly these and
// the mother offers exactly these, so a target can never name an asset that
// was not uploaded.
var Platforms = []Platform{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"windows", "amd64"},
	{"darwin", "arm64"},
}

// AssetName is the release asset for one platform. The version is not in the
// name because GitHub scopes assets to their release — the tag in the URL is
// what identifies the build.
//
// No .exe on Windows: the agent replaces its own executable at whatever path
// it already runs from, and both the download URL and the mother's index key
// on the bare name.
func AssetName(goos, goarch string) string {
	return assetPrefix + goos + "-" + goarch
}

// PlatformOf reads the platform back out of an asset name, reporting false for
// checksum companions, release notes, and anything not in the build matrix.
func PlatformOf(asset string) (string, bool) {
	rest, found := strings.CutPrefix(asset, assetPrefix)
	if !found || strings.HasSuffix(asset, ChecksumSuffix) {
		return "", false
	}
	for _, p := range Platforms {
		if rest == p.String() {
			return rest, true
		}
	}
	return "", false
}

// DownloadURL locates one asset of a tagged release.
func DownloadURL(baseURL, tag, asset string) string {
	return strings.TrimSuffix(baseURL, "/") + "/releases/download/" + tag + "/" + asset
}

// LatestDownloadURL is GitHub's moving pointer at the newest release, which is
// what the installer needs: it has no version to pin.
func LatestDownloadURL(baseURL, asset string) string {
	return strings.TrimSuffix(baseURL, "/") + "/releases/latest/download/" + asset
}

// ParseChecksum reads the expected digest from a .sha256 asset, accepting both
// a bare digest and the `<digest>  <filename>` form sha256sum writes.
//
// The hex is validated rather than compared as text: a 404 body or an HTML
// error page would otherwise become a "checksum" that simply never matches,
// reporting a corrupt download when the real fault was a missing asset.
func ParseChecksum(raw []byte) (string, error) {
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return "", fmt.Errorf("checksum file is empty")
	}
	sum := strings.ToLower(fields[0])
	if len(sum) != sha256HexLen {
		return "", fmt.Errorf("checksum %q is not a SHA-256 digest", truncate(sum))
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return "", fmt.Errorf("checksum %q is not hexadecimal", truncate(sum))
	}
	return sum, nil
}

func truncate(s string) string {
	if len(s) <= 32 {
		return s
	}
	return s[:32] + "…"
}
