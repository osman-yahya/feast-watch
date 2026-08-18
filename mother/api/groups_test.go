package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/store"
)

func dataInto(t *testing.T, body []byte, into any) {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if !env.Success {
		t.Fatalf("request failed: %s", env.Error)
	}
	if err := json.Unmarshal(env.Data, into); err != nil {
		t.Fatal(err)
	}
}

func TestGroupCRUD(t *testing.T) {
	a, _ := setup(t)

	w := adminReq(t, a.Handler(), http.MethodPost, "/api/groups", `{"name":"Veritabanı"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created store.Group
	dataInto(t, w.Body.Bytes(), &created)
	if created.ID == 0 || created.Name != "Veritabanı" {
		t.Fatalf("created: %+v", created)
	}

	w = adminReq(t, a.Handler(), http.MethodGet, "/api/groups", "")
	var list []store.Group
	dataInto(t, w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}

	w = adminReq(t, a.Handler(), http.MethodPatch,
		fmt.Sprintf("/api/groups/%d", created.ID), `{"name":"Veritabanı Sunucuları"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}

	w = adminReq(t, a.Handler(), http.MethodDelete, fmt.Sprintf("/api/groups/%d", created.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	w = adminReq(t, a.Handler(), http.MethodGet, "/api/groups", "")
	dataInto(t, w.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("group survived delete: %+v", list)
	}
}

// A name collision is the caller's to fix, so it must be distinguishable from
// a storage failure.
func TestCreateGroupRejectsDuplicateAndInvalidNames(t *testing.T) {
	a, _ := setup(t)
	adminReq(t, a.Handler(), http.MethodPost, "/api/groups", `{"name":"Prod"}`)

	w := adminReq(t, a.Handler(), http.MethodPost, "/api/groups", `{"name":"Prod"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate name: want 409, got %d %s", w.Code, w.Body)
	}
	w = adminReq(t, a.Handler(), http.MethodPost, "/api/groups", `{"name":"  "}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("blank name: want 400, got %d", w.Code)
	}
}

func TestSetGroupServersAndListMembers(t *testing.T) {
	a, st := setup(t)
	x, _ := st.AddServer("x")
	y, _ := st.AddServer("y")
	g, _ := st.CreateGroup("Prod")

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/groups/%d/servers", g.ID),
		fmt.Sprintf(`{"server_ids":[%d,%d]}`, x.ID, y.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("set members: %d %s", w.Code, w.Body)
	}

	w = adminReq(t, a.Handler(), http.MethodGet, fmt.Sprintf("/api/servers?group_id=%d", g.ID), "")
	var list []serverView
	dataInto(t, w.Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("filtered server list: %+v", list)
	}
}

// The panel renders each server's groups on the list, so they ride the row
// rather than needing a second call per server.
func TestServerListCarriesGroupMembership(t *testing.T) {
	a, st := setup(t)
	x, _ := st.AddServer("x")
	st.AddServer("y")
	g, _ := st.CreateGroup("Prod")
	st.SetGroupServers(g.ID, []int64{x.ID})

	w := adminReq(t, a.Handler(), http.MethodGet, "/api/servers", "")
	var list []serverView
	dataInto(t, w.Body.Bytes(), &list)

	byName := map[string]serverView{}
	for _, v := range list {
		byName[v.Name] = v
	}
	if len(byName["x"].Groups) != 1 || byName["x"].Groups[0].Name != "Prod" {
		t.Fatalf("x groups: %+v", byName["x"].Groups)
	}
	// An ungrouped server must carry an empty list, not null — the panel maps
	// over it.
	if byName["y"].Groups == nil {
		t.Fatal("an ungrouped server must carry an empty group list, not null")
	}
}

func TestGroupVersionRolloutTargetsEveryMember(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))
	x := reportedServer(t, a, st, "x", "linux", "amd64")
	y := reportedServer(t, a, st, "y", "linux", "amd64")
	g, _ := st.CreateGroup("Prod")
	st.SetGroupServers(g.ID, []int64{x.ID, y.ID})

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/groups/%d/version", g.ID), `{"version":"v1.3.0"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("group rollout: %d %s", w.Code, w.Body)
	}
	var result groupRolloutResult
	dataInto(t, w.Body.Bytes(), &result)
	if len(result.Applied) != 2 || len(result.Skipped) != 0 {
		t.Fatalf("rollout result: %+v", result)
	}
	for _, id := range []int64{x.ID, y.ID} {
		got, _ := st.ServerByID(id)
		if got.DesiredVersion != "v1.3.0" {
			t.Fatalf("server %d not targeted: %q", id, got.DesiredVersion)
		}
	}
}

// A group mixing platforms must not be all-or-nothing: one darwin laptop would
// otherwise permanently block a rollout across forty Linux hosts. The hosts
// that can take it are targeted and the rest are reported.
func TestGroupVersionRolloutSkipsMembersWithoutABuild(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))
	linux := reportedServer(t, a, st, "linux-box", "linux", "amd64")
	mac := reportedServer(t, a, st, "mac-box", "darwin", "arm64")
	g, _ := st.CreateGroup("Mixed")
	st.SetGroupServers(g.ID, []int64{linux.ID, mac.ID})

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/groups/%d/version", g.ID), `{"version":"v1.3.0"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a partial rollout is still a success: %d %s", w.Code, w.Body)
	}
	var result groupRolloutResult
	dataInto(t, w.Body.Bytes(), &result)
	if len(result.Applied) != 1 || result.Applied[0].Name != "linux-box" {
		t.Fatalf("applied: %+v", result.Applied)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Name != "mac-box" || result.Skipped[0].Reason == "" {
		t.Fatalf("skipped must name the host and the reason: %+v", result.Skipped)
	}
	got, _ := st.ServerByID(mac.ID)
	if got.DesiredVersion != "" {
		t.Fatal("a skipped host must not be targeted")
	}
}

// A fault in the VERSION itself — unpublished, or the moving alias — is not a
// per-host problem, so nothing is written at all.
func TestGroupVersionRolloutRefusesAVersionLevelFault(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))
	x := reportedServer(t, a, st, "x", "linux", "amd64")
	g, _ := st.CreateGroup("Prod")
	st.SetGroupServers(g.ID, []int64{x.ID})

	for _, version := range []string{"v9.9.9", "latest"} {
		w := adminReq(t, a.Handler(), http.MethodPut,
			fmt.Sprintf("/api/groups/%d/version", g.ID),
			fmt.Sprintf(`{"version":%q}`, version))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", version, w.Code, w.Body)
		}
		got, _ := st.ServerByID(x.ID)
		if got.DesiredVersion != "" {
			t.Fatalf("%s: nothing may be written on a version-level fault", version)
		}
	}
}

// Clearing is how a group rollout is cancelled, and it must reach every member
// regardless of platform.
func TestGroupVersionRolloutClearsEveryMember(t *testing.T) {
	a, st := setup(t)
	publish(t, a, build("v1.3.0", "linux-amd64"))
	x := reportedServer(t, a, st, "x", "linux", "amd64")
	mac := reportedServer(t, a, st, "mac", "darwin", "arm64")
	g, _ := st.CreateGroup("Prod")
	st.SetGroupServers(g.ID, []int64{x.ID, mac.ID})
	st.SetDesiredVersion(x.ID, "v1.3.0")
	st.SetDesiredVersion(mac.ID, "v1.3.0")

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/groups/%d/version", g.ID), `{"version":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	for _, id := range []int64{x.ID, mac.ID} {
		got, _ := st.ServerByID(id)
		if got.DesiredVersion != "" {
			t.Fatalf("server %d target not cleared: %q", id, got.DesiredVersion)
		}
	}
}

func TestGroupEndpointsRequireAPIKey(t *testing.T) {
	a, _ := setup(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/groups"},
		{http.MethodPost, "/api/groups"},
		{http.MethodPatch, "/api/groups/1"},
		{http.MethodDelete, "/api/groups/1"},
		{http.MethodPut, "/api/groups/1/servers"},
		{http.MethodPut, "/api/groups/1/version"},
	} {
		r := newRequest(tc.method, tc.path, `{"name":"x"}`)
		w := recorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: want 401, got %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestGroupEndpointsRejectUnknownGroup(t *testing.T) {
	a, _ := setup(t)
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPatch, "/api/groups/9999", `{"name":"x"}`},
		{http.MethodDelete, "/api/groups/9999", ""},
		{http.MethodPut, "/api/groups/9999/servers", `{"server_ids":[]}`},
		{http.MethodPut, "/api/groups/9999/version", `{"version":"v1.3.0"}`},
	} {
		w := adminReq(t, a.Handler(), tc.method, tc.path, tc.body)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s: want 404, got %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
}
