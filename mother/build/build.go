// Package build makes the mother the source of the binaries its fleet runs.
//
// The agents originally downloaded from GitHub Releases, and then through the
// mother mirroring those releases. Both put GitHub in the path: one for the
// bytes, the other for the tag that names them. Neither survives contact with
// the fleet this runs on, whose agents have no route off their network at all.
// So the mother compiles from a source tree on its own host and serves what it
// made, and the agents ask nothing but their mother.
//
// What that costs is worth stating where it is implemented rather than only in
// a design note. The mother's host now needs a Go toolchain and a copy of the
// source, and the mother becomes the sole authority for what a version means:
// nothing outside it computed the checksum an agent verifies against, and there
// is no published artifact to compare a build with afterwards. Reproducing a
// version means having the same source tree, not fetching the same file.
//
// What is deliberately kept is the identity rule that made the GitHub path
// safe: one version string names one set of bytes, forever. Build refuses to
// rebuild a version already in the catalogue, because agents compare versions
// as strings and a fleet cannot tell two builds of "v9.0.0" apart.
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/osman-yahya/feast-watch/shared/release"
)

// target is one binary to produce: which command, and for which platform.
type target struct {
	pkg    string
	asset  string
	goos   string
	goarch string
}

// targets is everything a complete version carries — the same set
// release.ExpectedAssets describes, which is what the catalogue is read back
// against.
func targets() []target {
	out := make([]target, 0, len(release.Platforms)+len(release.MotherPlatforms))
	for _, p := range release.Platforms {
		out = append(out, target{
			pkg:   "./agent/cmd/feast-watch-agent",
			asset: release.AssetName(p.GOOS, p.GOARCH),
			goos:  p.GOOS, goarch: p.GOARCH,
		})
	}
	for _, p := range release.MotherPlatforms {
		out = append(out, target{
			pkg:   "./mother/cmd/feast-watch",
			asset: release.MotherAssetName(p.GOOS, p.GOARCH),
			goos:  p.GOOS, goarch: p.GOARCH,
		})
	}
	return out
}

// Build compiles every platform from sourceDir and publishes them into
// outDir/version, with a checksum beside each binary.
//
// It builds into a staging directory and renames at the end, because the
// filesystem IS the catalogue: a version directory that appears while four of
// six platforms exist is a rollout target that resolves for some hosts and
// 404s for the rest. Renaming makes the version appear complete or not at all.
func Build(sourceDir, outDir, version string) error {
	version = strings.TrimSpace(version)
	if version == "" {
		return fmt.Errorf("a build needs a version to be known by")
	}
	if strings.ContainsAny(version, `/\`) || version == "." || version == ".." {
		// The version becomes a directory name and later a URL path segment.
		return fmt.Errorf("version %q may not contain a path separator", version)
	}

	final := filepath.Join(outDir, version)
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("version %s is already built: one version string must never name two different binaries", version)
	}

	staging := final + ".building"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	// Removed on every path but the successful rename below.
	defer os.RemoveAll(staging)

	// Checked once, before anything is staged. Without it the first target
	// fails with exec's own wording — `exec: "go": executable file not found in
	// $PATH` — wrapped in a message about building an asset, which reads like
	// the source tree is at fault rather than the host.
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("no Go toolchain on this host: the mother compiles what its fleet runs, " +
			"so it needs one — deploy/mother-install.sh installs it, or install Go and put it on PATH")
	}

	toolchain, err := toolchainEnv(outDir)
	if err != nil {
		return err
	}
	for _, t := range targets() {
		if err := compile(sourceDir, staging, version, t, toolchain); err != nil {
			return err
		}
	}
	return os.Rename(staging, final)
}

// toolchainCacheDir is where the Go toolchain's own caches go when the
// environment names none. Beside the catalogue, so it lands in the state
// directory the mother's unit already creates and owns.
const toolchainCacheDir = ".toolchain-cache"

// toolchainEnv gives the compiler somewhere to keep its caches.
//
// The mother's service account has no home directory — `useradd
// --no-create-home`, which is what an account that only runs a daemon should
// have — and the Go toolchain refuses to compile anything at all without one:
// it cannot place its build cache and stops before reading a single file. So
// the caches are named explicitly, and a build stops depending on a directory
// nobody meant to create.
//
// Anything the environment already names is left alone. An air-gapped mother
// gets its modules by having $GOMODCACHE seeded by hand, and overriding that
// would replace a populated cache with an empty one and turn a working build
// into a failed download.
func toolchainEnv(outDir string) ([]string, error) {
	env := []string{"CGO_ENABLED=0"}
	for _, cache := range []struct{ key, sub string }{
		{"GOCACHE", "build"},
		{"GOMODCACHE", "mod"},
	} {
		if os.Getenv(cache.key) != "" {
			continue
		}
		dir := filepath.Join(outDir, toolchainCacheDir, cache.sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("preparing %s: %w", cache.key, err)
		}
		env = append(env, cache.key+"="+dir)
	}
	return env, nil
}

func compile(sourceDir, staging, version string, t target, toolchain []string) error {
	out := filepath.Join(staging, t.asset)
	// The same flags the release pipeline used, for the same reasons: the
	// version has to be linked in or the binary reports "dev" forever and no
	// agent can satisfy a rollout target, and CGO off is what makes one build
	// run on every distribution the fleet happens to use.
	cmd := exec.Command("go", "build",
		"-ldflags", "-s -w -X github.com/osman-yahya/feast-watch/shared/version.Version="+version,
		"-o", out, t.pkg)
	cmd.Dir = sourceDir
	// A new slice per target rather than one appended to across the loop: they
	// differ only in GOOS/GOARCH, and sharing the backing array is how two
	// targets end up compiled for the same platform.
	cmd.Env = append(append(append([]string{}, os.Environ()...), toolchain...),
		"GOOS="+t.goos, "GOARCH="+t.goarch)

	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building %s: %w: %s", t.asset, err, strings.TrimSpace(string(combined)))
	}
	return writeChecksum(out)
}

// writeChecksum records the digest of what was actually produced. On this path
// the mother is the only party that will ever compute it, so it describes the
// file beside it rather than any external claim about it.
func writeChecksum(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	return os.WriteFile(path+release.ChecksumSuffix,
		[]byte(hex.EncodeToString(hash.Sum(nil))+"\n"), 0o644)
}
