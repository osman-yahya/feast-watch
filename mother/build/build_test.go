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

// The build's own caches live in the catalogue directory. They are not a
// version, and a panel offering ".toolchain-cache" as a rollout target would be
// offering something no agent could ever download.
func TestStoreIgnoresItsOwnHousekeeping(t *testing.T) {
	out := filepath.Join(t.TempDir(), "builds")

	t.Setenv("GOCACHE", "")
	t.Setenv("GOMODCACHE", "")
	if err := Build(tinySource(t, helloMain), out, "v9.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, toolchainCacheDir)); err != nil {
		t.Fatalf("this test is asserting about a directory that was not created: %v", err)
	}

	agents, _, _, err := New(out).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Version != "v9.0.0" {
		t.Fatalf("the catalogue listed something that is not a version: %+v", agents)
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

// The mother's service account is created with no home directory, which is what
// its unit wants — and what the Go toolchain will not tolerate: with no HOME it
// cannot place its build cache and refuses to compile anything at all. So the
// build supplies caches of its own, beside the catalogue, in the state
// directory the mother already owns.
//
// Without this, the one command that makes a fleet updatable fails on every
// correctly-installed host, and only there — never on the laptop it was written
// on.
func TestBuildSuppliesItsOwnToolchainCaches(t *testing.T) {
	out := filepath.Join(t.TempDir(), "builds")

	t.Setenv("HOME", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("GOCACHE", "")
	t.Setenv("GOMODCACHE", "")
	t.Setenv("GOPATH", "")

	if err := Build(tinySource(t, helloMain), out, "v9.0.0"); err != nil {
		t.Fatalf("a build must not need a home directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, toolchainCacheDir, "build")); err != nil {
		t.Fatalf("no build cache was placed beside the catalogue: %v", err)
	}
}

// An operator who names a cache keeps it. A host that pre-seeded $GOMODCACHE so
// `feast-watch build` needs no network — the whole point on an air-gapped
// mother — must not have the build quietly compile against an empty one it made
// itself.
func TestBuildKeepsTheCachesTheEnvironmentNames(t *testing.T) {
	out := filepath.Join(t.TempDir(), "builds")
	named := t.TempDir()

	t.Setenv("GOCACHE", filepath.Join(named, "build"))
	t.Setenv("GOMODCACHE", filepath.Join(named, "mod"))

	if err := Build(tinySource(t, helloMain), out, "v9.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(named, "build")); err != nil {
		t.Fatalf("the named GOCACHE went unused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, toolchainCacheDir)); !os.IsNotExist(err) {
		t.Fatalf("a cache was placed beside the catalogue despite the environment naming one: %v", err)
	}
}

// A host with no compiler is the one failure this command exists to make
// impossible, so when it happens anyway it has to say so in one line rather
// than six times over as `exec: "go": executable file not found in $PATH`.
func TestBuildNamesAMissingToolchain(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	err := Build(tinySource(t, helloMain), filepath.Join(t.TempDir(), "builds"), "v9.0.0")
	if err == nil {
		t.Fatal("a build with no toolchain must fail")
	}
	if !strings.Contains(err.Error(), "Go toolchain") {
		t.Fatalf("the error does not name what is missing: %v", err)
	}
}
