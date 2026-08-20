package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// releaseHost plays GitHub Releases and counts how many times the binary
// itself was transferred.
func releaseHost(t *testing.T, body []byte, sum string, hits *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			w.Write([]byte(sum + "\n"))
		case strings.Contains(r.URL.Path, "feast-watch-"):
			if hits != nil {
				atomic.AddInt64(hits, 1)
			}
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCache(t *testing.T, srv *httptest.Server) *Cache {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "binaries"), srv.URL, srv.Client())
}

func TestEnsureStoresTheBinaryAndItsChecksum(t *testing.T) {
	body := []byte("AGENT-BINARY")
	srv := releaseHost(t, body, digest(body), nil)
	c := newCache(t, srv)

	path, err := c.Ensure("v1.0.2", "feast-watch-agent-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("content: %q", got)
	}

	// The checksum is served alongside, because the agent verifies before it
	// replaces itself and would otherwise have nothing to verify against.
	sum, err := os.ReadFile(path + ".sha256")
	if err != nil {
		t.Fatalf("no checksum stored: %v", err)
	}
	if strings.TrimSpace(string(sum)) != digest(body) {
		t.Fatalf("checksum: %q", sum)
	}
}

// The whole point of a cache: a fleet rolling out to one version must not pull
// the same 12MB through the mother once per host.
func TestEnsureTransfersOnlyOnce(t *testing.T) {
	body := []byte("AGENT-BINARY")
	var hits int64
	srv := releaseHost(t, body, digest(body), &hits)
	c := newCache(t, srv)

	for i := 0; i < 3; i++ {
		if _, err := c.Ensure("v1.0.2", "feast-watch-agent-linux-amd64"); err != nil {
			t.Fatal(err)
		}
	}
	if hits != 1 {
		t.Fatalf("the binary was transferred %d times", hits)
	}
}

// A group rollout points every member at one version at once, so the first
// request for a build arrives many times over before any of them finishes.
func TestConcurrentEnsureTransfersOnlyOnce(t *testing.T) {
	body := []byte("AGENT-BINARY")
	var hits int64
	srv := releaseHost(t, body, digest(body), &hits)
	c := newCache(t, srv)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Ensure("v1.0.2", "feast-watch-agent-linux-amd64"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("%d concurrent callers caused %d transfers", 8, hits)
	}
}

// The mother verifies against what GitHub published before anything is stored,
// so a corrupted transfer never becomes something agents can download.
func TestEnsureRefusesAMismatchAndCachesNothing(t *testing.T) {
	srv := releaseHost(t, []byte("tampered"), digest([]byte("expected")), nil)
	c := newCache(t, srv)

	path, err := c.Ensure("v1.0.2", "feast-watch-agent-linux-amd64")
	if err == nil {
		t.Fatal("a binary that failed verification was cached")
	}
	if path != "" {
		t.Fatalf("path returned on failure: %q", path)
	}
	if entries, _ := os.ReadDir(c.dir); len(entries) != 0 {
		// A failure that leaves a directory behind makes the next call look
		// like a hit and serve nothing.
		t.Fatalf("cache is not empty after a failure: %v", entries)
	}
}

func TestEnsureFailsOnAnUnpublishedTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newCache(t, srv)

	if _, err := c.Ensure("v9.9.9", "feast-watch-agent-linux-amd64"); err == nil {
		t.Fatal("expected a failure for a tag with no build")
	}
}
