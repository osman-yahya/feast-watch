package release

import "testing"

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

func TestPlatformOfRoundTripsEveryBuiltPlatform(t *testing.T) {
	for _, p := range Platforms {
		plat, ok := PlatformOf(AssetName(p.GOOS, p.GOARCH))
		if !ok || plat != p.String() {
			t.Fatalf("%s did not round-trip: got %q ok=%v", p, plat, ok)
		}
	}
}

func TestPlatformOfRejectsCompanionsAndStrangers(t *testing.T) {
	for _, name := range []string{
		"feast-watch-agent-linux-amd64.sha256", // the checksum companion
		"feast-watch-agent",                    // no platform
		"feast-watch-agent-plan9-mips",         // not a platform we build
		"README.md",
		"",
	} {
		if plat, ok := PlatformOf(name); ok {
			t.Fatalf("%q must not parse as a build, got %q", name, plat)
		}
	}
}

func TestDownloadURLPinsTheTag(t *testing.T) {
	got := DownloadURL(DefaultBaseURL, "v1.3.0", AssetName("linux", "amd64"))
	want := "https://github.com/osman-yahya/feast-watch/releases/download/v1.3.0/feast-watch-agent-linux-amd64"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// The installer has no version to pin — it takes whatever is current — so it
// needs GitHub's moving pointer rather than a tag.
func TestLatestDownloadURL(t *testing.T) {
	got := LatestDownloadURL(DefaultBaseURL, AssetName("linux", "arm64"))
	want := "https://github.com/osman-yahya/feast-watch/releases/latest/download/feast-watch-agent-linux-arm64"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDownloadURLTolerARatesATrailingSlash(t *testing.T) {
	if got := DownloadURL(DefaultBaseURL+"/", "v1.3.0", "a"); got != DownloadURL(DefaultBaseURL, "v1.3.0", "a") {
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
