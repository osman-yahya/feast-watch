// Package build makes the mother the source of the binaries its fleet runs.
//
// The agents originally downloaded from GitHub Releases, and then through the
// mother mirroring those releases. Both put GitHub in the path: one for the
// bytes, the other for the tag that names them. This removes it entirely — the
// mother compiles from a source tree on its own host and serves what it made.
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

	for _, t := range targets() {
		if err := compile(sourceDir, staging, version, t); err != nil {
			return err
		}
	}
	return os.Rename(staging, final)
}

func compile(sourceDir, staging, version string, t target) error {
	out := filepath.Join(staging, t.asset)
	// The same flags the release pipeline used, for the same reasons: the
	// version has to be linked in or the binary reports "dev" forever and no
	// agent can satisfy a rollout target, and CGO off is what makes one build
	// run on every distribution the fleet happens to use.
	cmd := exec.Command("go", "build",
		"-ldflags", "-s -w -X github.com/osman-yahya/feast-watch/shared/version.Version="+version,
		"-o", out, t.pkg)
	cmd.Dir = sourceDir
	cmd.Env = append(os.Environ(),
		"GOOS="+t.goos, "GOARCH="+t.goarch, "CGO_ENABLED=0")

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
