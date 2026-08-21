package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	catalogue "github.com/osman-yahya/feast-watch/mother/build"
	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/store"
)

const agentAsset = "feast-watch-agent-linux-amd64"

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// catalogueAPI is a mother holding one built version, v1.0.2, in the catalogue
// on disk and in its release index — the state a real mother is in after
// `feast-watch build v1.0.2` and one poll.
//
// The files are written rather than compiled: what these routes do with a
// catalogue is not conditional on a Go toolchain being present, and requiring
// one to run the tests would put a cross-compile of six binaries in front of
// every run.
func catalogueAPI(t *testing.T, body []byte) *API {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	dir := filepath.Join(t.TempDir(), "builds")
	if err := os.MkdirAll(filepath.Join(dir, "v1.0.2"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, asset := range []string{agentAsset, "feast-watch-mother-linux-amd64"} {
		path := filepath.Join(dir, "v1.0.2", asset)
		if err := os.WriteFile(path, body, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".sha256", []byte(sha256Hex(body)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	a := New(st, "adminkey", withBothReleases(
		[]release.Build{build("v1.0.2", "linux-amd64")},
		[]release.Build{build("v1.0.2", "linux-amd64")},
	))
	// Imported under an alias: `build` is already versions_test.go's helper for
	// naming one published version, and the package that compiles them is the
	// newcomer here.
	a.SetBinarySource(catalogue.New(dir))
	return a
}

// The route shape is GitHub's on purpose: the agent builds its download URL
// with shared/release.DownloadURL and needed no change when the bytes stopped
// coming from a release host and started being compiled by the mother.
func TestServesABuildAtGitHubsURLShape(t *testing.T) {
	body := []byte("AGENT-BINARY")
	a := catalogueAPI(t, body)

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
	a := catalogueAPI(t, body)

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
	a := catalogueAPI(t, body)

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
// Only names this project actually builds may reach the filesystem.
func TestRefusesAnythingThatIsNotABuiltAsset(t *testing.T) {
	a := catalogueAPI(t, []byte("AGENT-BINARY"))

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

// A platform the index lists but the catalogue does not hold is a 404, not a
// 500 and not an empty 200: the agent asking has no other route to a binary,
// so the answer has to be one its error message can carry back to the panel.
func TestAVersionNotOnDiskIs404(t *testing.T) {
	a := catalogueAPI(t, []byte("AGENT-BINARY"))

	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/releases/download/v1.0.2/feast-watch-agent-linux-arm64", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

// The served installer points every new host at this mother, for the binary as
// well as for the config. It is the only address such a host can reach.
func TestInstallScriptNamesTheMotherForBinaries(t *testing.T) {
	a := catalogueAPI(t, []byte("AGENT-BINARY"))
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
	body := w.Body.String()
	if !strings.Contains(body, "MOTHER_URL=http://10.0.0.1:8443") {
		t.Fatalf("the installer does not name the mother:\n%s", body)
	}
	if !strings.Contains(body, `"$MOTHER_URL/releases/latest/download/$asset"`) {
		t.Fatalf("the installer does not download from the mother:\n%s", body)
	}
	if strings.Contains(body, "github.com") {
		t.Fatalf("the installer sends the host to the internet:\n%s", body)
	}
}

// A mother wired without its catalogue must say it holds no builds rather than
// answer with something. This is a wiring mistake rather than a deployment
// choice — every mother compiles and serves — so what it must not do is fail
// in a way that takes the monitoring down with it.
func TestDownloadRoutesAre404WithoutACatalogue(t *testing.T) {
	a, _ := motherAPI(t, nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/releases/download/v1.4.0/"+agentAsset, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
}
