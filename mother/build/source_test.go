package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarball builds a gzipped tar shaped like GitHub's source archive: every entry
// under one top-level directory named for the repo and ref.
func tarball(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: root + "/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serveTarball(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchSourceExtractsTheTreeWithoutItsTopLevelDirectory(t *testing.T) {
	body := tarball(t, "feast-watch-1.3.0", map[string]string{
		"go.mod":                              "module example.com/tiny\n\ngo 1.26\n",
		"agent/cmd/feast-watch-agent/main.go": helloMain,
	})
	srv := serveTarball(t, body)
	dir := filepath.Join(t.TempDir(), "src")

	if err := FetchSource(context.Background(), srv.Client(), srv.URL, "v1.3.0", dir); err != nil {
		t.Fatal(err)
	}
	// The archive's top-level directory is named for the ref, so keeping it
	// would make every build path depend on the version being built.
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("go.mod is not at the root of the extracted tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent/cmd/feast-watch-agent/main.go")); err != nil {
		t.Fatal(err)
	}
}

// An archive entry names its own destination, so a crafted one can name a path
// outside the directory being extracted into. This is the whole reason to
// resolve every entry rather than trusting it.
func TestFetchSourceRefusesAnEntryThatEscapes(t *testing.T) {
	for _, escape := range []string{"../escaped", "../../escaped", "/etc/escaped"} {
		body := tarball(t, "feast-watch-1.3.0", map[string]string{escape: "owned"})
		srv := serveTarball(t, body)
		dir := filepath.Join(t.TempDir(), "src")

		if err := FetchSource(context.Background(), srv.Client(), srv.URL, "v1.3.0", dir); err == nil {
			t.Fatalf("%q was extracted", escape)
		}
	}
}

func TestFetchSourceFailsOnARefThatIsNotPublished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	err := FetchSource(context.Background(), srv.Client(), srv.URL, "v9.9.9",
		filepath.Join(t.TempDir(), "src"))
	if err == nil {
		t.Fatal("expected a failure for a ref with no source archive")
	}
}

// The end this exists for: fetch a tree and build it, with nothing on the host
// but Go.
func TestFetchThenBuild(t *testing.T) {
	body := tarball(t, "feast-watch-9.0.0", map[string]string{
		"go.mod":                              "module example.com/tiny\n\ngo 1.26\n",
		"agent/cmd/feast-watch-agent/main.go": helloMain,
		"mother/cmd/feast-watch/main.go":      helloMain,
	})
	srv := serveTarball(t, body)

	src := filepath.Join(t.TempDir(), "src")
	if err := FetchSource(context.Background(), srv.Client(), srv.URL, "v9.0.0", src); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "builds")
	if err := Build(src, out, "v9.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "v9.0.0", "feast-watch-agent-linux-amd64")); err != nil {
		t.Fatal(err)
	}
}
