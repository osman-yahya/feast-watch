package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/mirror"
	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/store"
)

const agentAsset = "feast-watch-agent-linux-amd64"

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mirrorAPI is a mother that mirrors one published build, v1.0.2, and knows
// about it in its release index — the state a real mother reaches by polling.
func mirrorAPI(t *testing.T, body []byte) (*API, *httptest.Server) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			w.Write([]byte(sha256Hex(body) + "\n"))
		case strings.Contains(r.URL.Path, "feast-watch-"):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	a := New(st, "adminkey", withBothReleases(
		[]release.Build{build("v1.0.2", "linux-amd64")},
		[]release.Build{build("v1.0.2", "linux-amd64")},
	))
	a.SetBinaryMirror(mirror.New(filepath.Join(t.TempDir(), "binaries"), upstream.URL, upstream.Client()))
	return a, upstream
}

// The route shape is GitHub's on purpose: the agent builds its download URL
// with shared/release.DownloadURL and knows nothing about the mother being in
// the way, so pointing RELEASE_BASE_URL at the mother is the whole change.
func TestServesABuildAtGitHubsURLShape(t *testing.T) {
	body := []byte("AGENT-BINARY")
	a, _ := mirrorAPI(t, body)

	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/releases/download/v1.0.2/"+agentAsset, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if w.Body.String() != string(body) {
		t.Fatalf("body: %q", w.Body)
	}
}

// The agent verifies before replacing itself, so the companion has to be there
// too — and it has to describe the bytes the mother actually holds.
func TestServesTheChecksumCompanion(t *testing.T) {
	body := []byte("AGENT-BINARY")
	a, _ := mirrorAPI(t, body)

	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/releases/download/v1.0.2/"+agentAsset+".sha256", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if strings.TrimSpace(w.Body.String()) != sha256Hex(body) {
		t.Fatalf("checksum: %q", w.Body)
	}
}

// The installer has no version to pin, so it asks for the moving pointer.
func TestServesLatestByResolvingItAgainstTheIndex(t *testing.T) {
	body := []byte("AGENT-BINARY")
	a, _ := mirrorAPI(t, body)

	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/releases/latest/download/"+agentAsset, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if w.Body.String() != string(body) {
		t.Fatalf("body: %q", w.Body)
	}
}

// The tag and the asset both arrive from the network and both become a path.
// Only names this project actually publishes may reach the filesystem.
func TestRefusesAnythingThatIsNotAPublishedBuild(t *testing.T) {
	a, _ := mirrorAPI(t, []byte("AGENT-BINARY"))

	for _, path := range []string{
		"/releases/download/v1.0.2/../../etc/passwd",
		"/releases/download/v1.0.2/feast-watch-agent-plan9-mips",
		"/releases/download/v1.0.2/README.md",
		"/releases/download/v9.9.9/" + agentAsset,        // tag the index never saw
		"/releases/download/..%2f..%2fetc/" + agentAsset, // traversal in the tag
	} {
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code == http.StatusOK {
			t.Fatalf("%s was served", path)
		}
	}
}

// Which release host a freshly installed agent is pointed at is decided by
// whether this mother mirrors — and the agent needs no code to understand
// either answer, because RELEASE_BASE_URL was already its way of being told.
func TestInstallScriptNamesTheMotherWhenItMirrors(t *testing.T) {
	body := []byte("AGENT-BINARY")
	a, _ := mirrorAPI(t, body)
	a.SetPublicURL("http://10.0.0.1:8443")

	srv, err := a.st.AddServer("web-1")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/install/"+srv.Token+".sh", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "RELEASE_BASE_URL=http://10.0.0.1:8443") {
		t.Fatalf("the installer does not point at the mother:\n%s", w.Body)
	}
	if strings.Contains(w.Body.String(), "RELEASE_BASE_URL=https://github.com") {
		t.Fatal("the installer still names the public release host")
	}
}

// And without a mirror it names the public host, which is the arrangement the
// agents started with and the better one where it works.
func TestInstallScriptNamesGitHubWithoutAMirror(t *testing.T) {
	a, st := motherAPI(t, nil)
	a.SetPublicURL("http://10.0.0.1:8443")

	srv, err := st.AddServer("web-1")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/install/"+srv.Token+".sh", nil))

	if !strings.Contains(w.Body.String(), "RELEASE_BASE_URL=https://github.com/") {
		t.Fatalf("the installer does not name the public release host:\n%s", w.Body)
	}
}

// A mother that is not mirroring must say it holds no builds rather than
// answer with something.
func TestDownloadRoutesAre404WithoutAMirror(t *testing.T) {
	a, _ := motherAPI(t, nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/releases/download/v1.4.0/"+agentAsset, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
}
