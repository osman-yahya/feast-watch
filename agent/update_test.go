package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/osman-yahya/feast-watch/shared/release"
	"github.com/osman-yahya/feast-watch/shared/selfupdate"
)

// assetPath is where a correctly built agent looks for its own replacement on
// its mother: the tag, then the platform-keyed asset name.
func assetPath(tag string) string {
	return "/releases/download/" + tag + "/" + release.AssetName(runtime.GOOS, runtime.GOARCH)
}

// motherServer stands in for the mother, serving one version out of its build
// catalogue.
func motherServer(t *testing.T, tag string, binary []byte, sum string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case assetPath(tag):
			w.Write(binary)
		case assetPath(tag) + release.ChecksumSuffix:
			fmt.Fprintln(w, sum)
		default:
			http.NotFound(w, r)
		}
	}))
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// updateConfig is the whole of an agent's knowledge of where binaries live:
// the mother's URL, and nothing beside it to point anywhere else.
func updateConfig(motherURL string) Config {
	return Config{MotherURL: motherURL}
}

func TestSelfUpdateReplacesBinaryAndExits(t *testing.T) {
	binary := []byte("NEW BINARY")
	srv := motherServer(t, "v1.3.0", binary, sum(binary))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	exitCode := -1
	err := selfUpdate(updateConfig(srv.URL), "v1.3.0", target,
		func(c int) { exitCode = c }, &http.Client{Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "NEW BINARY" {
		t.Fatalf("binary not replaced: %q", got)
	}
	if exitCode != 0 {
		t.Fatalf("must exit 0 for systemd restart, got %d", exitCode)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("replacement is not executable: %v", info.Mode())
	}
}

// Every byte of an update comes from the mother, because that is the only host
// an agent can reach. This asserts the whole of the request path: nothing is
// fetched from anywhere but MOTHER_URL, and what is fetched from it is the
// binary and its checksum under the tag that was asked for.
func TestSelfUpdateDownloadsOnlyFromTheMother(t *testing.T) {
	binary := []byte("NEW BINARY")
	var got []string
	mother := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		switch r.URL.Path {
		case assetPath("v1.3.0"):
			w.Write(binary)
		case assetPath("v1.3.0") + release.ChecksumSuffix:
			fmt.Fprintln(w, sum(binary))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mother.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	if err := selfUpdate(updateConfig(mother.URL), "v1.3.0", target, func(int) {}, mother.Client()); err != nil {
		t.Fatal(err)
	}
	// The set rather than the order: which of the two is fetched first is
	// shared/selfupdate's business, and pinning it here would break that
	// package's freedom to reorder without telling us anything about where the
	// bytes came from.
	want := map[string]bool{
		assetPath("v1.3.0"):                          true,
		assetPath("v1.3.0") + release.ChecksumSuffix: true,
	}
	if len(got) != len(want) {
		t.Fatalf("the mother was asked for %v, want exactly the binary and its checksum", got)
	}
	for _, path := range got {
		if !want[path] {
			t.Fatalf("unexpected request to the mother: %q", path)
		}
	}
}

func TestSelfUpdateRefusesOnChecksumMismatch(t *testing.T) {
	srv := motherServer(t, "v1.3.0", []byte("TAMPERED"), sum([]byte("EXPECTED")))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	exited := false
	err := selfUpdate(updateConfig(srv.URL), "v1.3.0", target,
		func(int) { exited = true }, srv.Client())
	if err == nil {
		t.Fatal("a checksum mismatch must refuse the update")
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Fatalf("binary was replaced despite the mismatch: %q", got)
	}
	if exited {
		t.Fatal("must not exit after refusing")
	}
}

// A tag with no asset for this platform, or one never built on the mother at
// all, is a 404. It must surface as an error the mother can display, not as a
// corrupt install.
func TestSelfUpdateFailsOnMissingRelease(t *testing.T) {
	srv := motherServer(t, "v1.3.0", []byte("X"), sum([]byte("X")))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	err := selfUpdate(updateConfig(srv.URL), "v9.9.9", target, func(int) {}, srv.Client())
	if err == nil {
		t.Fatal("a missing release must error")
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Fatalf("binary touched on a failed update: %q", got)
	}
}

// A proxy or a misrouted request can answer with an HTML error page instead of
// a checksum. Treating that body as a digest would report "checksum mismatch"
// for what is really a missing asset, sending the operator after the wrong
// fault.
func TestSelfUpdateRejectsANonDigestChecksumBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "<!DOCTYPE html><html>Not Found</html>")
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	err := selfUpdate(updateConfig(srv.URL), "v1.3.0", target, func(int) {}, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("want a checksum-parse error, got %v", err)
	}
}

// A failed update must leave nothing behind: a stranded .new file is never
// swept by anything and accumulates on every retry.
func TestSelfUpdateLeavesNoTemporaryFileBehind(t *testing.T) {
	srv := motherServer(t, "v1.3.0", []byte("TAMPERED"), sum([]byte("EXPECTED")))
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	if err := selfUpdate(updateConfig(srv.URL), "v1.3.0", target, func(int) {}, srv.Client()); err == nil {
		t.Fatal("expected the update to fail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "feast-watch-agent" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("leftovers after a failed update: %v", names)
	}
}

// An oversized asset must be refused by size rather than read into memory: the
// agent runs on the hosts it monitors, some of which are small.
func TestSelfUpdateRefusesAnOversizedAsset(t *testing.T) {
	huge := make([]byte, selfupdate.MaxBinarySize+1)
	srv := motherServer(t, "v1.3.0", huge, sum(huge))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	if err := selfUpdate(updateConfig(srv.URL), "v1.3.0", target, func(int) {}, srv.Client()); err == nil {
		t.Fatal("an asset over the cap must be refused")
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Fatal("binary replaced by an oversized asset")
	}
}
