package release

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const releasesJSON = `[
  {"tag_name":"v1.4.0","draft":false,"prerelease":false,"assets":[
    {"name":"feast-watch-agent-linux-amd64"},
    {"name":"feast-watch-agent-linux-amd64.sha256"},
    {"name":"feast-watch-agent-linux-arm64"},
    {"name":"feast-watch-agent-linux-arm64.sha256"}]},
  {"tag_name":"v1.9.0","draft":false,"prerelease":false,"assets":[
    {"name":"feast-watch-agent-linux-amd64"},
    {"name":"feast-watch-agent-linux-amd64.sha256"}]},
  {"tag_name":"v1.10.0","draft":false,"prerelease":false,"assets":[
    {"name":"feast-watch-agent-linux-amd64"},
    {"name":"feast-watch-agent-linux-amd64.sha256"}]},
  {"tag_name":"v2.0.0-rc1","draft":false,"prerelease":true,"assets":[
    {"name":"feast-watch-agent-linux-amd64"},
    {"name":"feast-watch-agent-linux-amd64.sha256"}]},
  {"tag_name":"v3.0.0","draft":true,"prerelease":false,"assets":[
    {"name":"feast-watch-agent-linux-amd64"},
    {"name":"feast-watch-agent-linux-amd64.sha256"}]},
  {"tag_name":"v1.1.0","draft":false,"prerelease":false,"assets":[
    {"name":"feast-watch-agent-linux-amd64"}]}
]`

func apiServer(t *testing.T, body string, etag string, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		// GitHub rejects a request with no User-Agent outright.
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "no user agent", http.StatusForbidden)
			return
		}
		if etag != "" {
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}

func TestFetchIndexesPublishedReleasesOnly(t *testing.T) {
	srv := apiServer(t, releasesJSON, "", nil)
	defer srv.Close()

	builds, _, err := NewClient(srv.URL, false).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	got := map[string][]string{}
	for _, b := range builds {
		got[b.Version] = b.Platforms
	}
	if _, ok := got["v3.0.0"]; ok {
		t.Fatal("a draft release must not be offered")
	}
	if _, ok := got["v2.0.0-rc1"]; ok {
		t.Fatal("a prerelease must not be offered by default")
	}
	// A build with no .sha256 companion cannot be installed — the agent
	// verifies before replacing itself — so offering it is a button that
	// always fails.
	if _, ok := got["v1.1.0"]; ok {
		t.Fatal("a build without its checksum must not be offered")
	}
	if len(got["v1.4.0"]) != 2 {
		t.Fatalf("v1.4.0 platforms: %v", got["v1.4.0"])
	}
}

func TestFetchCanIncludePrereleases(t *testing.T) {
	srv := apiServer(t, releasesJSON, "", nil)
	defer srv.Close()

	builds, _, err := NewClient(srv.URL, true).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range builds {
		if b.Version == "v2.0.0-rc1" {
			return
		}
	}
	t.Fatal("prereleases were requested but not offered")
}

// Newest first, and numerically: a plain string sort puts v1.9.0 above
// v1.10.0, which is the order an operator reads as "latest".
func TestFetchOrdersNewestFirst(t *testing.T) {
	srv := apiServer(t, releasesJSON, "", nil)
	defer srv.Close()

	builds, _, err := NewClient(srv.URL, false).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v1.10.0", "v1.9.0", "v1.4.0"}
	if len(builds) != len(want) {
		t.Fatalf("got %d builds: %+v", len(builds), builds)
	}
	for i, v := range want {
		if builds[i].Version != v {
			t.Fatalf("order: %+v want %v", builds, want)
		}
	}
}

// Unauthenticated GitHub allows 60 requests an hour per IP, but a conditional
// request answered 304 is not counted. Without the ETag a 5-minute poll would
// spend a fifth of the hourly budget on a list that rarely changes.
func TestFetchSendsTheStoredETagAndReportsNotModified(t *testing.T) {
	hits := 0
	srv := apiServer(t, releasesJSON, `W/"abc123"`, &hits)
	defer srv.Close()

	client := NewClient(srv.URL, false)
	if _, notModified, err := client.Fetch(context.Background()); err != nil || notModified {
		t.Fatalf("first fetch: notModified=%v err=%v", notModified, err)
	}
	_, notModified, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !notModified {
		t.Fatal("second fetch must be answered 304 from the stored ETag")
	}
	if hits != 2 {
		t.Fatalf("expected two requests, got %d", hits)
	}
}

func TestFetchSurfacesAnAPIFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	if _, _, err := NewClient(srv.URL, false).Fetch(context.Background()); err == nil {
		t.Fatal("a 403 must surface as an error, not as an empty release list")
	}
}

func TestFetchRejectsAMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<!DOCTYPE html>"))
	}))
	defer srv.Close()

	if _, _, err := NewClient(srv.URL, false).Fetch(context.Background()); err == nil {
		t.Fatal("a non-JSON body must error")
	}
}

func TestReleasesJSONFixtureIsValid(t *testing.T) {
	var v []any
	if err := json.Unmarshal([]byte(releasesJSON), &v); err != nil {
		t.Fatal(err)
	}
}
