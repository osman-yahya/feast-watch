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

// fetchInstallScript renders the install script for a freshly created server.
func fetchInstallScript(t *testing.T, a *API, st *store.Store, name string) string {
	t.Helper()
	srv, err := st.AddServer(name)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/install/"+srv.Token+".sh", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	return w.Body.String()
}

// A mother served without TLS must not hand agents an https:// URL it never
// listens on — the installed agent would fail every push.
func TestInstallScriptRendersSchemeFromMother(t *testing.T) {
	a, st := setup(t)
	a.SetPublicAddr("10.0.0.1:8443")
	a.SetScheme("http")

	body := fetchInstallScript(t, a, st, "Plain_HTTP")
	if !strings.Contains(body, "MOTHER_URL=http://10.0.0.1:8443") {
		t.Fatalf("script must use the mother's own scheme:\n%s", body)
	}
	if strings.Contains(body, "https://10.0.0.1:8443") {
		t.Fatalf("script must not hardcode https:\n%s", body)
	}
}

// With a self-signed certificate the mother must tell the agent to skip
// verification; install.sh is the only channel that reaches agent.conf.
func TestInstallScriptEmitsTLSSkipVerify(t *testing.T) {
	a, st := setup(t)
	a.SetPublicAddr("10.0.0.1:8443")
	a.SetAgentTLSSkipVerify(true)

	body := fetchInstallScript(t, a, st, "Self_Signed")
	if !strings.Contains(body, "TLS_SKIP_VERIFY=true") {
		t.Fatalf("script must write TLS_SKIP_VERIFY into agent.conf:\n%s", body)
	}
}

// Default (a publicly-trusted cert) must not weaken the agent's trust.
func TestInstallScriptOmitsTLSSkipVerifyByDefault(t *testing.T) {
	a, st := setup(t)
	a.SetPublicAddr("10.0.0.1:8443")

	body := fetchInstallScript(t, a, st, "Trusted_Cert")
	if strings.Contains(body, "TLS_SKIP_VERIFY") {
		t.Fatalf("script must not disable verification unless asked:\n%s", body)
	}
}

func TestInstallCommandUsesScheme(t *testing.T) {
	if got := InstallCommand("http", "10.0.0.1:8443", "tk_abc"); got != "curl -sSLk http://10.0.0.1:8443/install/tk_abc.sh | sudo bash" {
		t.Fatalf("http scheme: %q", got)
	}
	if got := InstallCommand("https", "10.0.0.1:8443", "tk_abc"); got != "curl -sSLk https://10.0.0.1:8443/install/tk_abc.sh | sudo bash" {
		t.Fatalf("https scheme: %q", got)
	}
}
