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
)

// assetPath is where a correctly built agent looks for its own replacement on
// the release host: the tag, then the platform-keyed asset name.
func assetPath(tag string) string {
	return "/releases/download/" + tag + "/" + release.AssetName(runtime.GOOS, runtime.GOARCH)
}

// releaseServer stands in for github.com, serving one tagged release.
func releaseServer(t *testing.T, tag string, binary []byte, sum string) *httptest.Server {
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

func updateConfig(baseURL string) Config {
	return Config{MotherURL: "http://mother.invalid", ReleaseBaseURL: baseURL}
}

func TestSelfUpdateReplacesBinaryAndExits(t *testing.T) {
	binary := []byte("NEW BINARY")
	srv := releaseServer(t, "v1.3.0", binary, sum(binary))
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

// The binary is downloaded from the release host, never from the mother. The
// mother only ever names a version.
func TestSelfUpdateNeverTouchesTheMother(t *testing.T) {
	binary := []byte("NEW BINARY")
	srv := releaseServer(t, "v1.3.0", binary, sum(binary))
	defer srv.Close()

	var motherHits int
	mother := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		motherHits++
		http.NotFound(w, r)
	}))
	defer mother.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	cfg := Config{MotherURL: mother.URL, ReleaseBaseURL: srv.URL}
	if err := selfUpdate(cfg, "v1.3.0", target, func(int) {}, srv.Client()); err != nil {
		t.Fatal(err)
	}
	if motherHits != 0 {
		t.Fatalf("the mother was asked for the binary %d times", motherHits)
	}
}

func TestSelfUpdateRefusesOnChecksumMismatch(t *testing.T) {
	srv := releaseServer(t, "v1.3.0", []byte("TAMPERED"), sum([]byte("EXPECTED")))
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

// A tag with no asset for this platform, or no release at all, is a 404. It
// must surface as an error the mother can display, not as a corrupt install.
func TestSelfUpdateFailsOnMissingRelease(t *testing.T) {
	srv := releaseServer(t, "v1.3.0", []byte("X"), sum([]byte("X")))
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

// GitHub answers a release download with a 404 HTML page when the asset is
// absent. Treating that body as a digest would report "checksum mismatch" for
// what is really a missing asset, sending the operator after the wrong fault.
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
	srv := releaseServer(t, "v1.3.0", []byte("TAMPERED"), sum([]byte("EXPECTED")))
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
	huge := make([]byte, maxBinarySize+1)
	srv := releaseServer(t, "v1.3.0", huge, sum(huge))
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
