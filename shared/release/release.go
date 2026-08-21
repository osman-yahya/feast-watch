// Package release is the single source of truth for how agent builds are named
// and where they are downloaded from.
//
// It is imported by the agent (to build its own download URL), by the mother
// (to name what it compiles and to index its own catalogue) and by the
// installer template the mother serves. Before this existed the mother's
// platform list and the release script's had already drifted, with a comment
// saying they must agree as the only enforcement.
package release

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// DefaultSourceRepoURL is the repository the mother fetches SOURCE from when it
// is told to build a version it has no local checkout of.
//
// It is the only address in this project that points at the internet, and only
// the mother's own host ever resolves it. No agent has it: agents download from
// their mother and nothing else (mother/api/binaries.go), which is what makes
// "an agent never leaves the private network" a property of the code rather
// than of how somebody configured it.
const DefaultSourceRepoURL = "https://github.com/osman-yahya/feast-watch"

// assetPrefix is the stem every agent asset shares.
const assetPrefix = "feast-watch-agent-"

// motherAssetPrefix is the stem every mother asset shares. A separate prefix,
// not a suffix or a directory, so one string comparison tells the two families
// apart at every point a name is read.
const motherAssetPrefix = "feast-watch-mother-"

// Kind is which binary an asset holds. The mother indexes both families out of
// the same release and must never offer one where the other is meant: an agent
// handed a mother build would replace itself with the control plane.
type Kind string

const (
	KindAgent  Kind = "agent"
	KindMother Kind = "mother"
)

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

// MotherPlatforms is the mother's build matrix, deliberately narrower than
// Platforms. The mother is a systemd service on a Linux host — the unit file,
// deploy/ and the promote helper all assume it — so a darwin or windows build
// would be a rollout target no supported deployment could apply.
var MotherPlatforms = []Platform{
	{"linux", "amd64"},
	{"linux", "arm64"},
}

// MotherAssetName is the release asset holding the mother for one platform.
// Same shape as AssetName, and for the same reason: GitHub scopes assets to
// their release, so the tag in the URL is what identifies the build.
func MotherAssetName(goos, goarch string) string {
	return motherAssetPrefix + goos + "-" + goarch
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

// ExpectedAssets is every file a complete release carries: one binary per
// platform in the build matrix, each with its checksum companion.
//
// It exists so the release workflow can assert what it published instead of
// trusting that four independent matrix legs all got there. They did not
// once: a leg that failed its version check while the other three had already
// uploaded left a release with six of eight assets, and the missing one was
// linux-amd64 — the platform the served installer fetches. Nothing noticed for
// 84 minutes, because a release is "created" the moment its first asset lands.
func ExpectedAssets() []string {
	out := make([]string, 0, 2*(len(Platforms)+len(MotherPlatforms)))
	for _, p := range Platforms {
		asset := AssetName(p.GOOS, p.GOARCH)
		out = append(out, asset, asset+ChecksumSuffix)
	}
	for _, p := range MotherPlatforms {
		asset := MotherAssetName(p.GOOS, p.GOARCH)
		out = append(out, asset, asset+ChecksumSuffix)
	}
	return out
}

// AssetKindOf reads the family and platform back out of an asset name,
// reporting false for checksum companions, release notes, and any platform
// that family is not built for.
//
// It replaced PlatformOf when the mother became publishable: a single name
// space with one prefix could not answer "is this an agent build" without the
// caller re-parsing the prefix it had just skipped.
func AssetKindOf(asset string) (Kind, string, bool) {
	if strings.HasSuffix(asset, ChecksumSuffix) {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(asset, motherAssetPrefix); found {
		return KindMother, rest, builtFor(MotherPlatforms, rest)
	}
	if rest, found := strings.CutPrefix(asset, assetPrefix); found {
		return KindAgent, rest, builtFor(Platforms, rest)
	}
	return "", "", false
}

func builtFor(platforms []Platform, plat string) bool {
	for _, p := range platforms {
		if p.String() == plat {
			return true
		}
	}
	return false
}

// DownloadURL locates one asset of a built version on the host serving it.
//
// The path shape is GitHub's, kept deliberately: the mother's download routes
// answer it (mother/api/binaries.go), so the agent and the installer build the
// same URL they always did against an address that never leaves the private
// network.
func DownloadURL(baseURL, tag, asset string) string {
	return strings.TrimSuffix(baseURL, "/") + "/releases/download/" + tag + "/" + asset
}

// LatestDownloadURL is the moving pointer at the newest build the mother holds,
// which is what the installer needs: it has no version to pin.
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
