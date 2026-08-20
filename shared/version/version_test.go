package version

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The flag every build path must carry. Spelled out from the package's own
// import path rather than hardcoded, because the failure being guarded is a
// WRONG path: the linker accepts -X for a symbol that does not exist without a
// word of complaint, and the binary then ships the default.
const versionSymbol = "github.com/osman-yahya/feast-watch/shared/version.Version="

// buildDefinitions are the files that compile a shippable binary.
//
// There are four of them and nothing but this test makes them agree. That is
// not hypothetical: docker-compose.yml passed `args: { VERSION: ... }` to both
// Dockerfiles and documented it as "so the binary reports a real version
// instead of the dev default", while neither Dockerfile declared the ARG or
// used it — Docker drops an unconsumed build arg in silence, so every
// containerised agent reported "dev" for as long as the file existed, with the
// compose file's own comment insisting otherwise.
//
// A binary reporting "dev" is not a cosmetic problem. The panel shows it as
// the agent's version, and a rollout compares desired against reported
// (mother/api/admin.go updateState), so a "dev" agent can never converge on
// any target an operator sets.
var buildDefinitions = []struct {
	path string
	why  string
}{
	{"Dockerfile.agent", "every containerised agent, including the k8s DaemonSet"},
	{"Dockerfile.mother", "the mother's own reported version (GET /api/version)"},
	{"bin/release.sh", "locally built binaries, which is what deploy/mother-install.sh installs"},
	{"mother/build/build.go", "every binary this mother compiles and serves to its own fleet"},
}

// .github/workflows/release.yml was the fourth, and is gone: this project no
// longer publishes GitHub releases, because it no longer depends on GitHub for
// anything. mother/build/build.go took over the job it did — and inherits the
// same obligation, which is why it is listed above rather than trusted.

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func TestEveryBuildPathInjectsTheVersion(t *testing.T) {
	for _, def := range buildDefinitions {
		t.Run(def.path, func(t *testing.T) {
			if !strings.Contains(repoFile(t, def.path), versionSymbol) {
				t.Fatalf("%s does not inject the version with -X %s\n"+
					"binaries it produces report %q forever, which breaks %s",
					def.path, versionSymbol, Version, def.why)
			}
		})
	}
}

// The compose file hands a VERSION build arg to both images. An arg no
// Dockerfile declares is dropped without an error, so the two halves of that
// wiring are asserted against each other rather than left to agree by luck —
// which is exactly how they came to disagree.
func TestComposeVersionArgIsDeclaredByTheDockerfiles(t *testing.T) {
	compose := repoFile(t, "docker-compose.yml")
	if !strings.Contains(compose, "VERSION:") {
		t.Skip("docker-compose.yml no longer passes a VERSION build arg")
	}
	for _, path := range []string{"Dockerfile.agent", "Dockerfile.mother"} {
		if !strings.Contains(repoFile(t, path), "ARG VERSION") {
			t.Fatalf("docker-compose.yml passes a VERSION build arg that %s never declares; "+
				"Docker discards it silently and the image reports %q", path, Version)
		}
	}
}

// The default is what a build path that forgot the flag produces, so it has to
// be recognisable as "nobody set this" rather than look like a version.
func TestDefaultVersionIsObviouslyNotARelease(t *testing.T) {
	if strings.HasPrefix(Version, "v") {
		t.Fatalf("the default version %q reads like a release; an unstamped binary "+
			"must be distinguishable from a stamped one at a glance", Version)
	}
}
