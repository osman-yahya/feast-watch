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
	"testing"
	"time"
)

// buildPath is where a correctly built agent looks for its own replacement:
// version plus the full GOOS-GOARCH platform, matching what bin/release.sh
// stages on the mother.
const buildPath = "/download/agent/v1.3.0-" + runtime.GOOS + "-" + runtime.GOARCH

func updateServer(t *testing.T, binary []byte, sum string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case buildPath:
			w.Write(binary)
		case buildPath + ".sha256":
			fmt.Fprintln(w, sum)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSelfUpdateReplacesBinaryAndExits(t *testing.T) {
	binary := []byte("NEW BINARY")
	h := sha256.Sum256(binary)
	srv := updateServer(t, binary, hex.EncodeToString(h[:]))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	exitCode := -1
	err := selfUpdate(Config{MotherURL: srv.URL}, "v1.3.0", target, func(c int) { exitCode = c }, &http.Client{Timeout: 60 * time.Second})
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
}

// The mother also stages a legacy "<version>-<arch>" name for agents installed
// before builds were platform-explicit. A current agent must not fall back to
// it: on a host whose GOOS differs from that build's, the download is not an
// executable this machine can run, and installing it takes the agent down with
// no way back except touching the host.
func TestSelfUpdateDoesNotFallBackToArchOnlyBuild(t *testing.T) {
	binary := []byte("BINARY FOR ANOTHER OS")
	h := sha256.Sum256(binary)
	sum := hex.EncodeToString(h[:])
	archOnly := "/download/agent/v1.3.0-" + runtime.GOARCH

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case archOnly:
			w.Write(binary)
		case archOnly + ".sha256":
			fmt.Fprintln(w, sum)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	err := selfUpdate(Config{MotherURL: srv.URL}, "v1.3.0", target,
		func(int) { t.Fatal("must not exit") }, &http.Client{Timeout: 60 * time.Second})
	if err == nil {
		t.Fatal("a build without this agent's GOOS must not satisfy the update")
	}
	if got, _ := os.ReadFile(target); string(got) != "OLD" {
		t.Fatalf("binary must be untouched, got %q", got)
	}
}

func TestSelfUpdateRejectsBadChecksum(t *testing.T) {
	srv := updateServer(t, []byte("NEW BINARY"), "deadbeef")
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	err := selfUpdate(Config{MotherURL: srv.URL}, "v1.3.0", target, func(int) { t.Fatal("must not exit") }, &http.Client{Timeout: 60 * time.Second})
	if err == nil {
		t.Fatal("checksum mismatch must fail")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "OLD" {
		t.Fatal("binary must be untouched on checksum failure")
	}
}
