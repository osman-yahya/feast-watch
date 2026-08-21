package release

import "testing"

// motherURL stands in for the one address an agent has. Every download URL in
// this project is built against its mother, so that is what these assert
// against — there is no public host left in the path.
const motherURL = "http://10.0.0.1:8443"

func TestAssetNameIsPlatformKeyed(t *testing.T) {
	if got := AssetName("linux", "amd64"); got != "feast-watch-agent-linux-amd64" {
		t.Fatalf("asset name: %q", got)
	}
	// No .exe: the agent replaces its own executable at whatever path it runs
	// from, and both the download URL and the mother's index key on the bare
	// name.
	if got := AssetName("windows", "amd64"); got != "feast-watch-agent-windows-amd64" {
		t.Fatalf("windows asset name: %q", got)
	}
}

func TestDownloadURLPinsTheTag(t *testing.T) {
	got := DownloadURL(motherURL, "v1.3.0", AssetName("linux", "amd64"))
	want := "http://10.0.0.1:8443/releases/download/v1.3.0/feast-watch-agent-linux-amd64"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// The installer has no version to pin — it takes whatever the mother has built
// most recently — so it needs the moving pointer rather than a tag.
func TestLatestDownloadURL(t *testing.T) {
	got := LatestDownloadURL(motherURL, AssetName("linux", "arm64"))
	want := "http://10.0.0.1:8443/releases/latest/download/feast-watch-agent-linux-arm64"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDownloadURLTolerARatesATrailingSlash(t *testing.T) {
	if got := DownloadURL(motherURL+"/", "v1.3.0", "a"); got != DownloadURL(motherURL, "v1.3.0", "a") {
		t.Fatalf("trailing slash changed the URL: %q", got)
	}
}

func TestParseChecksumAcceptsBothShasumForms(t *testing.T) {
	const want = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	for _, raw := range []string{
		want + "\n",
		want + "  feast-watch-agent-linux-amd64\n", // sha256sum output
		"  " + want + "  \n",
	} {
		got, err := ParseChecksum([]byte(raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got != want {
			t.Fatalf("%q -> %q", raw, got)
		}
	}
}

func TestParseChecksumRejectsAnythingElse(t *testing.T) {
	for _, raw := range []string{"", "   ", "not-hex-at-all", "abc123", "<!DOCTYPE html>"} {
		if got, err := ParseChecksum([]byte(raw)); err == nil {
			t.Fatalf("%q must be rejected, got %q", raw, got)
		}
	}
}

// The agent, the mother's index and the CI matrix all have to agree on which
// platforms exist; disagreement means the mother offers a target whose asset
// was never uploaded and the agent 404s in a loop.
func TestPlatformsIsTheBuildMatrix(t *testing.T) {
	if len(Platforms) == 0 {
		t.Fatal("no platforms declared")
	}
	seen := map[string]bool{}
	for _, p := range Platforms {
		if seen[p.String()] {
			t.Fatalf("duplicate platform %s", p)
		}
		seen[p.String()] = true
	}
	if !seen["linux-amd64"] {
		t.Fatal("linux-amd64 must be built: it is what the installer fetches")
	}
}

// The release workflow asserts that a finished release carries exactly these
// names. Deriving them here rather than listing them in YAML is what keeps the
// assertion honest: adding a platform to Platforms extends the check by
// construction, instead of leaving CI content with a release missing a build.
func TestExpectedAssetsCoversEveryPlatformWithItsChecksum(t *testing.T) {
	got := ExpectedAssets()
	if len(got) != 2*(len(Platforms)+len(MotherPlatforms)) {
		t.Fatalf("expected a binary and a checksum per build, got %d names for %d agent + %d mother platforms: %v",
			len(got), len(Platforms), len(MotherPlatforms), got)
	}
	have := make(map[string]bool, len(got))
	for _, name := range got {
		if have[name] {
			t.Fatalf("duplicate asset name %q", name)
		}
		have[name] = true
	}
	for _, p := range Platforms {
		asset := AssetName(p.GOOS, p.GOARCH)
		if !have[asset] {
			t.Fatalf("missing agent binary %q", asset)
		}
		if !have[asset+ChecksumSuffix] {
			t.Fatalf("missing checksum for %q", asset)
		}
	}
	for _, p := range MotherPlatforms {
		asset := MotherAssetName(p.GOOS, p.GOARCH)
		if !have[asset] {
			t.Fatalf("missing mother binary %q", asset)
		}
		if !have[asset+ChecksumSuffix] {
			t.Fatalf("missing checksum for %q", asset)
		}
	}
}

func TestMotherAssetNameIsPlatformKeyed(t *testing.T) {
	if got := MotherAssetName("linux", "amd64"); got != "feast-watch-mother-linux-amd64" {
		t.Fatalf("mother asset name: %q", got)
	}
}

// The two families must never be confused for one another: an agent handed a
// mother build would replace itself with the control plane.
func TestAssetKindOfSeparatesTheFamilies(t *testing.T) {
	for _, tc := range []struct {
		asset string
		kind  Kind
		plat  string
	}{
		{"feast-watch-agent-linux-amd64", KindAgent, "linux-amd64"},
		{"feast-watch-agent-darwin-arm64", KindAgent, "darwin-arm64"},
		{"feast-watch-mother-linux-amd64", KindMother, "linux-amd64"},
		{"feast-watch-mother-linux-arm64", KindMother, "linux-arm64"},
	} {
		kind, plat, ok := AssetKindOf(tc.asset)
		if !ok || kind != tc.kind || plat != tc.plat {
			t.Fatalf("%q -> (%q, %q, %v)", tc.asset, kind, plat, ok)
		}
	}
}

func TestAssetKindOfRejectsCompanionsAndStrangers(t *testing.T) {
	for _, name := range []string{
		"feast-watch-agent-linux-amd64.sha256",  // the checksum companion
		"feast-watch-mother-linux-amd64.sha256", // ditto
		"feast-watch-agent",                     // no platform
		"feast-watch-mother",                    // no platform
		"feast-watch-mother-darwin-arm64",       // not a platform the mother is built for
		"feast-watch-agent-plan9-mips",
		"README.md",
		"",
	} {
		if kind, plat, ok := AssetKindOf(name); ok {
			t.Fatalf("%q must not parse as a build, got (%q, %q)", name, kind, plat)
		}
	}
}

func TestAssetKindOfRoundTripsEveryBuiltPlatform(t *testing.T) {
	for _, p := range Platforms {
		kind, plat, ok := AssetKindOf(AssetName(p.GOOS, p.GOARCH))
		if !ok || kind != KindAgent || plat != p.String() {
			t.Fatalf("agent %s did not round-trip: (%q, %q, %v)", p, kind, plat, ok)
		}
	}
	for _, p := range MotherPlatforms {
		kind, plat, ok := AssetKindOf(MotherAssetName(p.GOOS, p.GOARCH))
		if !ok || kind != KindMother || plat != p.String() {
			t.Fatalf("mother %s did not round-trip: (%q, %q, %v)", p, kind, plat, ok)
		}
	}
}

// The mother runs as a systemd service on a Linux host: deploy/, the unit and
// the promote helper all assume it. Offering a darwin mother build would be a
// rollout target no supported deployment could apply.
func TestMotherIsBuiltForLinuxOnly(t *testing.T) {
	for _, p := range MotherPlatforms {
		if p.GOOS != "linux" {
			t.Fatalf("mother platform %s is not linux", p)
		}
	}
}
