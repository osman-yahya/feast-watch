package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseHost serves one asset and its checksum at the URL shape the mother
// answers — GitHub's, kept when the bytes moved home.
func releaseHost(t *testing.T, body []byte, sum string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			w.Write([]byte(sum + "  asset\n"))
		case strings.HasSuffix(r.URL.Path, "/asset"):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPlaceWritesAnExecutableWhenTheChecksumMatches(t *testing.T) {
	body := []byte("#!/bin/true\n")
	srv := releaseHost(t, body, digest(body))
	dest := filepath.Join(t.TempDir(), "feast-watch")

	if err := Place(srv.Client(), srv.URL, "v1.4.0", "asset", dest); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("content: %q", got)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v — a replacement that is not executable cannot be started", info.Mode())
	}
}

// The whole point of the checksum: a corrupted or substituted binary must not
// reach the destination, and must not be left lying beside it either — nothing
// else ever sweeps a temporary file, so a stranded one accumulates per retry.
func TestPlaceRefusesAMismatchAndLeavesNothingBehind(t *testing.T) {
	srv := releaseHost(t, []byte("tampered"), digest([]byte("expected")))
	dir := t.TempDir()
	dest := filepath.Join(dir, "feast-watch")

	err := Place(srv.Client(), srv.URL, "v1.4.0", "asset", dest)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err: %v", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("a binary that failed verification was placed anyway")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// The checksum is fetched first, and it is small: a tag that was never
// published, or one with no build for this platform, fails for the price of
// one request instead of after a whole binary transfer.
func TestPlaceFailsOnAMissingChecksumBeforeTransferringAnything(t *testing.T) {
	var assetHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".sha256") {
			assetHits++
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "feast-watch")
	if err := Place(srv.Client(), srv.URL, "v9.9.9", "asset", dest); err == nil {
		t.Fatal("expected a failure for an unpublished tag")
	}
	if assetHits != 0 {
		t.Fatalf("the binary was requested %d times despite no checksum", assetHits)
	}
}

// An oversized asset is refused by size rather than read into memory. The
// agent runs on the hosts it monitors, some of which are small, and the mother
// stages into a state directory it does not want to fill either.
func TestPlaceRefusesAnOversizedAsset(t *testing.T) {
	huge := make([]byte, MaxBinarySize+1)
	srv := releaseHost(t, huge, digest(huge))
	dir := t.TempDir()
	dest := filepath.Join(dir, "feast-watch")
	if err := os.WriteFile(dest, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Place(srv.Client(), srv.URL, "v1.4.0", "asset", dest); err == nil {
		t.Fatal("an asset over the cap must be refused")
	}
	if got, _ := os.ReadFile(dest); string(got) != "OLD" {
		t.Fatal("the destination was replaced by an oversized asset")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}
