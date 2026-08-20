package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/store"
)

// stubTarget stands in for the updater. The API needs to know only two things:
// whether this deployment can promote a staged binary, and what it is.
type stubTarget struct {
	supported bool
	platform  string
}

func (s stubTarget) Supported() bool  { return s.supported }
func (s stubTarget) Platform() string { return s.platform }

// motherAPI publishes v1.4.0 for linux-amd64 only, so a target for any other
// platform is a real rejection rather than an arranged one. A nil target leaves
// the updater unwired, which is what a mother built without one has.
func motherAPI(t *testing.T, target MotherUpdateTarget) (*API, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	a := New(st, "adminkey", withBothReleases(
		[]release.Build{build("v1.4.0", "linux-amd64")},
		[]release.Build{build("v1.4.0", "linux-amd64")},
	))
	if target != nil {
		a.SetMotherUpdate(target)
	}
	return a, st
}

func TestSetMotherVersionAcceptsAPublishedBuildForThisPlatform(t *testing.T) {
	a, st := motherAPI(t, stubTarget{supported: true, platform: "linux-amd64"})

	w := adminReq(t, a.Handler(), http.MethodPut, "/api/mother/version", `{"version":"v1.4.0"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "v1.4.0" {
		t.Fatalf("row: %+v", row)
	}
}

func TestSetMotherVersionRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  stubTarget
		payload string
		want    string
	}{
		{"the moving alias", stubTarget{true, "linux-amd64"}, `{"version":"latest"}`, "moving alias"},
		{"an unpublished version", stubTarget{true, "linux-amd64"}, `{"version":"v9.9.9"}`, "no published release"},
		{"a platform with no build", stubTarget{true, "linux-arm64"}, `{"version":"v1.4.0"}`, "no v1.4.0 mother build for linux-arm64"},
		{"a deployment that cannot promote", stubTarget{false, "linux-amd64"}, `{"version":"v1.4.0"}`, "not available on this deployment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := motherAPI(t, tc.target)
			w := adminReq(t, a.Handler(), http.MethodPut, "/api/mother/version", tc.payload)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("body %s does not explain the refusal (%q)", w.Body, tc.want)
			}
			row, err := st.MotherUpdate()
			if err != nil {
				t.Fatal(err)
			}
			if row.DesiredVersion != "" {
				t.Fatalf("a refused target was written anyway: %+v", row)
			}
		})
	}
}

// Cancelling must work even when the version being cancelled is one the release
// index no longer offers — that is often exactly why it is being cancelled.
func TestSetMotherVersionAcceptsAnEmptyVersionAsCancellation(t *testing.T) {
	a, st := motherAPI(t, stubTarget{supported: true, platform: "linux-amd64"})
	if err := st.SetMotherDesiredVersion("v9.9.9", 100); err != nil {
		t.Fatal(err)
	}

	w := adminReq(t, a.Handler(), http.MethodPut, "/api/mother/version", `{"version":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "" {
		t.Fatalf("row: %+v", row)
	}
}

func TestGetVersionReportsTheMotherUpdateState(t *testing.T) {
	a, st := motherAPI(t, stubTarget{supported: true, platform: "linux-amd64"})

	view := getVersion(t, a)
	if view.MotherUpdateState != "idle" {
		t.Fatalf("state: %q", view.MotherUpdateState)
	}
	if view.MotherPlatform != "linux-amd64" {
		t.Fatalf("platform: %q", view.MotherPlatform)
	}
	if len(view.MotherBuilds) != 1 || view.MotherBuilds[0].Version != "v1.4.0" {
		t.Fatalf("mother builds: %+v", view.MotherBuilds)
	}

	if err := st.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	if view := getVersion(t, a); view.MotherUpdateState != "pending" || view.MotherDesiredVersion != "v1.4.0" {
		t.Fatalf("view: %+v", view)
	}

	// Failing clears the target and keeps the reason, so a failed update has no
	// desired version left to distinguish it from idle — the error does.
	if err := st.FailMotherUpdate("gave up on v1.4.0 after 3 attempts"); err != nil {
		t.Fatal(err)
	}
	view = getVersion(t, a)
	if view.MotherUpdateState != "failed" {
		t.Fatalf("state: %q", view.MotherUpdateState)
	}
	if !strings.Contains(view.MotherUpdateError, "gave up") {
		t.Fatalf("error: %q", view.MotherUpdateError)
	}
}

func TestGetVersionReportsUnsupportedWhereItIs(t *testing.T) {
	a, _ := motherAPI(t, stubTarget{supported: false, platform: "linux-amd64"})
	if view := getVersion(t, a); view.MotherUpdateState != "unsupported" {
		t.Fatalf("state: %q", view.MotherUpdateState)
	}
}

// A mother with no updater wired at all must read as unsupported rather than
// panic: that is every test in this package, and any build without one.
func TestGetVersionWithoutAnUpdaterIsUnsupported(t *testing.T) {
	a, _ := motherAPI(t, nil)
	if view := getVersion(t, a); view.MotherUpdateState != "unsupported" {
		t.Fatalf("state: %q", view.MotherUpdateState)
	}
}

// The agent half of the payload is what every existing panel reads; extending
// it must not move any of it.
func TestGetVersionKeepsTheAgentFieldsIntact(t *testing.T) {
	a, _ := motherAPI(t, stubTarget{supported: true, platform: "linux-amd64"})
	view := getVersion(t, a)
	if len(view.Agents) != 1 || view.Agents[0].Version != "v1.4.0" {
		t.Fatalf("agents: %+v", view.Agents)
	}
	if view.MotherVersion == "" {
		t.Fatal("mother_version must always be reported")
	}
}
