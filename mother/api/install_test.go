package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/store"
)

func TestInstallScriptRendersTokenAndMotherURL(t *testing.T) {
	a, st := setup(t)
	a.SetPublicAddr("10.0.0.1:8443")
	srv, _ := st.AddServer("DB_Sunucusu")

	r := httptest.NewRequest(http.MethodGet, "/install/"+srv.Token+".sh", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"MOTHER_URL=https://10.0.0.1:8443", "TOKEN=" + srv.Token, `SERVER_NAME="DB_Sunucusu"`, "systemctl"} {
		if !strings.Contains(body, want) {
			t.Fatalf("script missing %q:\n%s", want, body)
		}
	}

	r = httptest.NewRequest(http.MethodGet, "/install/tk_bogus.sh", nil)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown token: want 404, got %d", w.Code)
	}
}

func TestDownloadServesBinaryAndChecksum(t *testing.T) {
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "feast-watch-agent-v1.3.0"), []byte("BINARY"), 0o755)
	os.WriteFile(filepath.Join(dir, "feast-watch-agent-v1.3.0.sha256"), []byte("abc123\n"), 0o644)
	a := New(st, "adminkey", dir)

	r := httptest.NewRequest(http.MethodGet, "/download/agent/v1.3.0", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "BINARY" {
		t.Fatalf("binary download: %d %q", w.Code, w.Body.String())
	}

	r = httptest.NewRequest(http.MethodGet, "/download/agent/..%2F..%2Fetc%2Fpasswd", nil)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("path traversal must be rejected")
	}
}
