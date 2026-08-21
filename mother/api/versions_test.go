package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

// publish makes these builds the mother's release index, standing in for what
// a read of the build catalogue would have found. Which directories qualify as
// a build — one holding a binary and the checksum beside it — is decided and
// tested in mother/build; here the index is a given.
func publish(t *testing.T, a *API, builds ...release.Build) {
	t.Helper()
	a.releases = withReleases(builds...)
}

// build is a shorthand for one published version and the platforms it covers.
func build(version string, platforms ...string) release.Build {
	return release.Build{Version: version, Platforms: platforms}
}

// reportedServer creates a server and has it push once, so the mother knows
// its platform — the state every real server reaches within one interval.
func reportedServer(t *testing.T, a *API, st *store.Store, name, goos, arch string) store.Server {
	t.Helper()
	srv, err := st.AddServer(name)
	if err != nil {
		t.Fatal(err)
	}
	postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: name, AgentVersion: "v1.2.0", Hostname: "h", IP: "10.0.0.7",
		OS: goos, Arch: arch, Samples: map[string]float64{},
	})
	got, err := st.ServerByID(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func getVersion(t *testing.T, a *API) versionView {
	t.Helper()
	w := adminReq(t, a.Handler(), http.MethodGet, "/api/version", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var env envelope
	json.Unmarshal(w.Body.Bytes(), &env)
	var view versionView
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func TestVersionEndpointReportsMotherVersion(t *testing.T) {
	a, _ := setup(t)
	// The build injects this via -ldflags; unset it is "dev", which is still a
	// value the panel must be able to display rather than an empty cell.
	if getVersion(t, a).MotherVersion == "" {
		t.Fatal("mother version must always be reported")
	}
}

func TestVersionEndpointPassesTheIndexThrough(t *testing.T) {
	a, _ := setup(t)
	publish(t, a,
		build("v1.10.0", "linux-amd64", "linux-arm64"),
		build("v1.9.0", "linux-amd64"))

	agents := getVersion(t, a).Agents
	if len(agents) != 2 {
		t.Fatalf("builds = %+v, want 2 versions", agents)
	}
	// v1.10.0 is newer than v1.9.0; plain string ordering gets this backwards.
	if agents[0].Version != "v1.10.0" || agents[1].Version != "v1.9.0" {
		t.Fatalf("version order = %+v", agents)
	}
	if len(agents[0].Platforms) != 2 {
		t.Fatalf("platforms = %v, want both", agents[0].Platforms)
	}
}

// The index says when it was last checked and whether that check failed, so an
// operator picking a target can see the list may have moved on.
func TestVersionEndpointReportsIndexFreshness(t *testing.T) {
	a, _ := setup(t)
	if got := getVersion(t, a); !got.Stale {
		t.Fatal("a mother that has never reached the release host must report stale")
	}
	publish(t, a, build("v1.3.0", "linux-amd64"))
	got := getVersion(t, a)
	if got.Stale {
		t.Fatal("a populated index must not report stale")
	}
	if got.CheckedAt.IsZero() {
		t.Fatal("checked_at must be reported")
	}
}

func TestSetServerVersionReachesTheAgent(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))
	srv := reportedServer(t, a, st, "web-1", "linux", "amd64")

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/servers/%d/version", srv.ID), `{"version":"v1.3.0"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	// The ingest rate limiter allows one push per server per 2s, so read the
	// stored target rather than pushing again.
	got, _ := st.ServerByID(srv.ID)
	if got.DesiredVersion != "v1.3.0" {
		t.Fatalf("desired version = %q", got.DesiredVersion)
	}
}

// Per-server targets are the whole point: one host is updated and observed
// before the rest of the fleet follows.
func TestSetServerVersionLeavesOtherServersAlone(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))
	canary := reportedServer(t, a, st, "canary", "linux", "amd64")
	other := reportedServer(t, a, st, "other", "linux", "amd64")

	adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/servers/%d/version", canary.ID), `{"version":"v1.3.0"}`)

	got, _ := st.ServerByID(other.ID)
	if got.DesiredVersion != "" {
		t.Fatalf("untargeted server picked up a rollout: %q", got.DesiredVersion)
	}
}

func TestSetServerVersionRejectsAnUnpublishedVersion(t *testing.T) {
	a, st := setup(t)
	srv := reportedServer(t, a, st, "web-1", "linux", "amd64")

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/servers/%d/version", srv.ID), `{"version":"v9.9.9"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a version nobody built, got %d: %s", w.Code, w.Body)
	}
	got, _ := st.ServerByID(srv.ID)
	if got.DesiredVersion != "" {
		t.Fatal("a rejected target must not be stored")
	}
}

func TestSetServerVersionRejectsLatest(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build(latestAlias, "linux-amd64"))
	srv := reportedServer(t, a, st, "web-1", "linux", "amd64")

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/servers/%d/version", srv.ID), `{"version":"latest"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("targeting the moving alias must 400, got %d: %s", w.Code, w.Body)
	}
}

// The version exists, but not for this host's platform — installing it would
// hand the agent a binary this machine cannot execute.
func TestSetServerVersionRejectsWrongPlatform(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))
	srv := reportedServer(t, a, st, "arm-box", "linux", "arm64")

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/servers/%d/version", srv.ID), `{"version":"v1.3.0"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a missing platform build, got %d: %s", w.Code, w.Body)
	}
}

// Agents installed before builds carried a GOOS never report an architecture.
// Requiring one would lock those hosts out of the very update that teaches
// them to send it.
func TestSetServerVersionAllowsUnknownPlatform(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))
	srv, _ := st.AddServer("legacy")

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/servers/%d/version", srv.ID), `{"version":"v1.3.0"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for an agent that has not reported its platform, got %d: %s", w.Code, w.Body)
	}
}

// Clearing the target is how an operator cancels a rollout that has not landed.
func TestSetServerVersionAcceptsEmptyToCancel(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))
	srv := reportedServer(t, a, st, "web-1", "linux", "amd64")
	st.SetDesiredVersion(srv.ID, "v1.3.0")

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/servers/%d/version", srv.ID), `{"version":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	got, _ := st.ServerByID(srv.ID)
	if got.DesiredVersion != "" {
		t.Fatalf("target not cleared: %q", got.DesiredVersion)
	}
}

func TestSetServerVersionUnknownServer(t *testing.T) {
	a, _ := setup(t)
	w := adminReq(t, a.Handler(), http.MethodPut, "/api/servers/404/version", `{"version":"v1.3.0"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body)
	}
}

// The panel renders the rollout state rather than re-deriving it, so the rule
// lives here and is exercised through the surface the panel actually reads.
func TestServerListExposesUpdateState(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))

	reportedServer(t, a, st, "idle-box", "linux", "amd64")
	pending := reportedServer(t, a, st, "pending-box", "linux", "amd64")
	failed := reportedServer(t, a, st, "failed-box", "linux", "amd64")

	st.SetDesiredVersion(pending.ID, "v1.3.0")
	st.SetDesiredVersion(failed.ID, "v1.3.0")
	st.TouchServer(failed.ID, store.Heartbeat{
		AgentVersion: "v1.2.0", UpdateError: "checksum mismatch"}, 200)

	w := adminReq(t, a.Handler(), http.MethodGet, "/api/servers", "")
	var env envelope
	json.Unmarshal(w.Body.Bytes(), &env)
	var list []serverView
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatal(err)
	}

	byName := map[string]serverView{}
	for _, v := range list {
		byName[v.Name] = v
	}
	if got := byName["idle-box"].UpdateState; got != "idle" {
		t.Fatalf("no target set must read as idle, got %q", got)
	}
	if got := byName["pending-box"].UpdateState; got != "pending" {
		t.Fatalf("target set and not reached must read as pending, got %q", got)
	}
	if got := byName["failed-box"]; got.UpdateState != "failed" || got.UpdateError == "" {
		t.Fatalf("a failed update must surface with its reason: %+v", got)
	}
}

// Reaching the target ends the rollout: the row must fall back to idle rather
// than showing a target forever.
func TestUpdateStateReturnsToIdleWhenAgentReachesTarget(t *testing.T) {
	srv := store.Server{AgentVersion: "v1.3.0", DesiredVersion: "v1.3.0"}
	if got := updateState(srv); got != "idle" {
		t.Fatalf("agent on target must be idle, got %q", got)
	}
}

func TestVersionEndpointsRequireAPIKey(t *testing.T) {
	a, _ := setup(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/version"},
		{http.MethodPut, "/api/servers/1/version"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"version":"v1.3.0"}`))
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: want 401, got %d", tc.method, tc.path, w.Code)
		}
	}
}
