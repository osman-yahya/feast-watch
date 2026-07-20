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
)

func updateServer(t *testing.T, binary []byte, sum string) *httptest.Server {
	t.Helper()
	versionArch := "/download/agent/v1.3.0-" + runtime.GOARCH
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case versionArch:
			w.Write(binary)
		case versionArch + ".sha256":
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
	err := selfUpdate(Config{MotherURL: srv.URL}, "v1.3.0", target, func(c int) { exitCode = c })
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

func TestSelfUpdateRejectsBadChecksum(t *testing.T) {
	srv := updateServer(t, []byte("NEW BINARY"), "deadbeef")
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	err := selfUpdate(Config{MotherURL: srv.URL}, "v1.3.0", target, func(int) { t.Fatal("must not exit") })
	if err == nil {
		t.Fatal("checksum mismatch must fail")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "OLD" {
		t.Fatal("binary must be untouched on checksum failure")
	}
}
