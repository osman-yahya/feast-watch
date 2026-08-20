package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osman-yahya/feast-watch/shared/release"
)

// tinySource writes a module shaped like this repository — the two commands the
// mother builds — but trivial, so a test can exercise the real toolchain
// without compiling the real tree six times.
func tinySource(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/tiny\n\ngo 1.26\n")
	write("agent/cmd/feast-watch-agent/main.go", body)
	write("mother/cmd/feast-watch/main.go", body)
	return dir
}

const helloMain = "package main\n\nfunc main() {}\n"

func digestOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestBuildProducesEveryAssetWithItsChecksum(t *testing.T) {
	out := filepath.Join(t.TempDir(), "builds")
	if err := Build(tinySource(t, helloMain), out, "v9.0.0"); err != nil {
		t.Fatal(err)
	}

	for _, name := range release.ExpectedAssets() {
		path := filepath.Join(out, "v9.0.0", name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	// The checksum has to describe the binary beside it: the agent verifies
	// against it before replacing itself, and here the mother is the only
	// authority that will ever have computed it.
	binary := filepath.Join(out, "v9.0.0", release.AssetName("linux", "amd64"))
	sum, err := os.ReadFile(binary + release.ChecksumSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(sum)) != digestOf(t, binary) {
		t.Fatalf("checksum does not describe the binary: %q", sum)
	}
}

// The filesystem is the catalogue, so a version directory must never exist in a
// half-built state: a rollout target that resolves to four of six platforms is
// worse than one that does not resolve at all.
func TestAFailedBuildLeavesNoVersionBehind(t *testing.T) {
	out := filepath.Join(t.TempDir(), "builds")
	if err := Build(tinySource(t, "package main\n\nfunc main() { this is not go }\n"), out, "v9.0.0"); err == nil {
		t.Fatal("a source tree that does not compile must fail the build")
	}
	if _, err := os.Stat(filepath.Join(out, "v9.0.0")); err == nil {
		t.Fatal("a failed build left a version directory in the catalogue")
	}
}

func TestBuildRefusesToOverwriteAPublishedVersion(t *testing.T) {
	src := tinySource(t, helloMain)
	out := filepath.Join(t.TempDir(), "builds")
	if err := Build(src, out, "v9.0.0"); err != nil {
		t.Fatal(err)
	}
	// Rebuilding a version already in the catalogue would make one version
	// string name two different binaries — the exact damage a moved tag does,
	// and agents compare versions as strings.
	if err := Build(src, out, "v9.0.0"); err == nil {
		t.Fatal("rebuilding an existing version must be refused")
	}
}

func TestStoreListsWhatWasBuilt(t *testing.T) {
	out := filepath.Join(t.TempDir(), "builds")
	if err := Build(tinySource(t, helloMain), out, "v9.0.0"); err != nil {
		t.Fatal(err)
	}

	agents, mother, notModified, err := New(out).Fetch(context.Background())
	if err != nil || notModified {
		t.Fatalf("fetch: %v notModified=%v", err, notModified)
	}
	if len(agents) != 1 || agents[0].Version != "v9.0.0" {
		t.Fatalf("agents: %+v", agents)
	}
	if len(agents[0].Platforms) != len(release.Platforms) {
		t.Fatalf("agent platforms: %+v", agents[0].Platforms)
	}
	if len(mother) != 1 || len(mother[0].Platforms) != len(release.MotherPlatforms) {
		t.Fatalf("mother: %+v", mother)
	}
}

func TestStoreIsEmptyBeforeAnythingIsBuilt(t *testing.T) {
	agents, mother, _, err := New(filepath.Join(t.TempDir(), "nothing")).Fetch(context.Background())
	if err != nil {
		t.Fatalf("an empty catalogue is not an error: %v", err)
	}
	if len(agents) != 0 || len(mother) != 0 {
		t.Fatalf("agents=%+v mother=%+v", agents, mother)
	}
}

func TestEnsureResolvesABuiltAssetAndRefusesAnythingElse(t *testing.T) {
	out := filepath.Join(t.TempDir(), "builds")
	if err := Build(tinySource(t, helloMain), out, "v9.0.0"); err != nil {
		t.Fatal(err)
	}
	s := New(out)

	path, err := s.Ensure("v9.0.0", release.AssetName("linux", "amd64"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure("v9.9.9", release.AssetName("linux", "amd64")); err == nil {
		t.Fatal("a version that was never built must not resolve")
	}
}
