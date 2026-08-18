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
	a.SetPublicURL("http://10.0.0.1:8443")
	srv, _ := st.AddServer("DB_Sunucusu")

	r := httptest.NewRequest(http.MethodGet, "/install/"+srv.Token+".sh", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"MOTHER_URL=http://10.0.0.1:8443", "TOKEN=" + srv.Token, `SERVER_NAME="DB_Sunucusu"`, "systemctl"} {
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

// A freshly constructed API must not be able to hand agents a scheme the
// mother does not serve. There is no setter that raises the scheme on its own:
// the whole URL is supplied or the plain-HTTP default stands.
func TestInstallScriptDefaultsToPlainHTTP(t *testing.T) {
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	a := New(st, "adminkey", t.TempDir())

	body := fetchInstallScript(t, a, st, "Default_Mother")
	if !strings.Contains(body, "MOTHER_URL=http://127.0.0.1:8443") {
		t.Fatalf("default must be plain HTTP:\n%s", body)
	}
	if strings.Contains(body, "https://") {
		t.Fatalf("nothing may render an https URL by default:\n%s", body)
	}
}

// The mother serves plain HTTP, so the agent has no TLS knobs left to be told
// about. A leftover TLS_SKIP_VERIFY in a generated config would be an
// instruction to trust anything, carried to a host that never needs it.
func TestInstallScriptNeverWeakensTLS(t *testing.T) {
	a, st := setup(t)
	a.SetPublicURL("http://10.0.0.1:8443")

	body := fetchInstallScript(t, a, st, "No_TLS_Knobs")
	for _, forbidden := range []string{"TLS_SKIP_VERIFY", "CA_FILE", "curl -sSLk", "-fsSLk"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("installer must not carry %q:\n%s", forbidden, body)
		}
	}
}

// A mother fronted by something that terminates TLS is addressed over https,
// including a path prefix. The agent concatenates onto whatever it is given.
func TestInstallScriptCarriesAFrontingProxyURL(t *testing.T) {
	a, st := setup(t)
	a.SetPublicURL("https://ops.feast.tr/watch")

	body := fetchInstallScript(t, a, st, "Behind_Proxy")
	if !strings.Contains(body, "MOTHER_URL=https://ops.feast.tr/watch") {
		t.Fatalf("proxy URL must survive verbatim:\n%s", body)
	}
}

// The one-liner an operator pastes must reach the same endpoint the installed
// agent will use, and must not carry -k: with the mother on plain HTTP there is
// no certificate to excuse, and a fronting proxy is expected to present one the
// host already trusts.
func TestInstallCommandUsesThePublicURL(t *testing.T) {
	if got := InstallCommand("http://10.0.0.1:8443", "tk_abc"); got != "curl -sSL http://10.0.0.1:8443/install/tk_abc.sh | sudo bash" {
		t.Fatalf("plain: %q", got)
	}
	if got := InstallCommand("https://ops.feast.tr/watch", "tk_abc"); got != "curl -sSL https://ops.feast.tr/watch/install/tk_abc.sh | sudo bash" {
		t.Fatalf("behind proxy: %q", got)
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
