package api

import (
	"io/fs"
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

// `curl -sSL -o file` exits 0 on an HTTP 404 and writes the response body, so
// without --fail a missing build lands "404 page not found" in
// /usr/local/bin/feast-watch-agent, gets chmod 0755, and systemd respawns
// against it every 5 seconds. set -euo pipefail does not catch it: curl
// succeeded.
func TestInstallScriptFailsOnDownloadError(t *testing.T) {
	a, st := setup(t)
	body := fetchInstallScript(t, a, st, "Download_Guard")

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "curl ") {
			continue
		}
		if !strings.Contains(line, "-f") && !strings.Contains(line, "--fail") {
			t.Fatalf("every curl in the installer must fail on HTTP errors:\n%s", trimmed)
		}
	}
}

// The installer is consumed as `curl ... | sudo bash`, which executes each
// statement as it arrives. A connection dropped mid-transfer would otherwise
// run a prefix of the script — enabling a service whose binary was never
// written. Wrapping the body in main() and calling it on the last line means a
// truncated download never invokes anything.
func TestInstallScriptIsTruncationSafe(t *testing.T) {
	a, st := setup(t)
	body := fetchInstallScript(t, a, st, "Truncation_Guard")

	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last != `main "$@"` {
		t.Fatalf(`installer must end with 'main "$@"', got %q`, last)
	}
	if !strings.Contains(body, "main() {") {
		t.Fatalf("installer body must live inside main():\n%s", body)
	}
}

// Only mother/api/install.sh.tmpl is embedded (mother/api/install.go). A second
// copy elsewhere is never rendered, drifts silently, and is a coin-flip for
// anyone told to "change the installer".
func TestNoOrphanShellTemplates(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(root, "mother", "api")

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".sh.tmpl") {
			return nil
		}
		if filepath.Dir(path) != canonical {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("shell template outside mother/api/ is never embedded and will drift: %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
