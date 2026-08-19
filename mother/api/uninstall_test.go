package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

// clearPushLimit forgets the per-server rate-limit state. Ingest allows one
// push per server per 2 seconds; these tests need consecutive pushes to
// observe what changes between them, and sleeping through the gap would add
// seconds to the suite for nothing.
func clearPushLimit(a *API) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastPush = map[int64]time.Time{}
}

// pushed drives one server to "has reported at least once", which is what
// separates a host with an agent on it from a row nobody ever installed.
func pushed(t *testing.T, a *API, srv store.Server) {
	t.Helper()
	w := postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: srv.Name, Samples: map[string]float64{"cpu.usage": 1},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("seed push: %d %s", w.Code, w.Body)
	}
}

func deleteServer(t *testing.T, a *API, id int64, query string) *httptest.ResponseRecorder {
	t.Helper()
	return adminReq(t, a.Handler(), http.MethodDelete, fmt.Sprintf("/api/servers/%d%s", id, query), "")
}

func postUninstalled(t *testing.T, a *API, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/uninstalled", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

// Deleting a live server does not drop the row: it schedules the agent's own
// removal and keeps the row so the operator can watch it happen — and so the
// agent still has a token to push with while it does.
func TestDeleteRequestsUninstallOnALiveServer(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	pushed(t, a, srv)

	w := deleteServer(t, a, srv.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	got, err := st.ServerByID(srv.ID)
	if err != nil {
		t.Fatalf("row must survive a delete of a live server: %v", err)
	}
	if got.UninstallRequestedAt == 0 {
		t.Fatal("delete did not schedule the uninstall")
	}
}

// The command reaches the agent the only way anything can: in the answer to
// its own push.
func TestIngestTellsAScheduledAgentToUninstall(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	pushed(t, a, srv)

	clearPushLimit(a)
	var before protocol.IngestResponse
	json.Unmarshal(postIngest(t, a.Handler(), srv.Token,
		protocol.IngestRequest{Server: "web-1", Samples: map[string]float64{}}).Body.Bytes(), &before)
	if before.Uninstall {
		t.Fatal("an agent nobody deleted was told to remove itself")
	}

	deleteServer(t, a, srv.ID, "")
	clearPushLimit(a)

	var after protocol.IngestResponse
	json.Unmarshal(postIngest(t, a.Handler(), srv.Token,
		protocol.IngestRequest{Server: "web-1", Samples: map[string]float64{}}).Body.Bytes(), &after)
	if !after.Uninstall {
		t.Fatalf("scheduled agent was not told to remove itself: %+v", after)
	}
}

// The removal is confirmed by the uninstaller itself, after it has actually
// removed everything — which is the only report that means "gone from the
// host" rather than "told to go".
func TestUninstalledReportDropsTheRow(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	pushed(t, a, srv)
	deleteServer(t, a, srv.ID, "")

	if w := postUninstalled(t, a, srv.Token); w.Code != http.StatusOK {
		t.Fatalf("uninstalled report: %d %s", w.Code, w.Body)
	}
	if _, err := st.ServerByID(srv.ID); err != store.ErrNotFound {
		t.Fatalf("row survived the confirmed removal: %v", err)
	}
}

// The token authenticates, it does not authorise self-deletion: an agent that
// was never scheduled cannot delete its own server row (and with it every
// metric ever collected from that host).
func TestUninstalledReportRefusedWhenNobodyAskedForIt(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	pushed(t, a, srv)

	w := postUninstalled(t, a, srv.Token)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 for an unscheduled uninstall report, got %d %s", w.Code, w.Body)
	}
	if _, err := st.ServerByID(srv.ID); err != nil {
		t.Fatalf("row must survive: %v", err)
	}
}

func TestUninstalledReportRejectsABadToken(t *testing.T) {
	a, _ := setup(t)
	if w := postUninstalled(t, a, "tk_nope"); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if w := postUninstalled(t, a, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a token, got %d", w.Code)
	}
}

// A host that will never push again would strand its row in "uninstalling"
// forever, so the operator keeps a way out that does not wait for an agent.
func TestForceDeleteDropsTheRowImmediately(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	pushed(t, a, srv)

	if w := deleteServer(t, a, srv.ID, "?force=true"); w.Code != http.StatusOK {
		t.Fatalf("force delete: %d %s", w.Code, w.Body)
	}
	if _, err := st.ServerByID(srv.ID); err != store.ErrNotFound {
		t.Fatalf("force delete left the row: %v", err)
	}
}

// A server that never pushed has no agent to talk to. Scheduling an uninstall
// for it would wait for a push that is not coming, so it is dropped outright.
func TestDeleteOfANeverPushedServerIsImmediate(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("never-installed")

	w := deleteServer(t, a, srv.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if _, err := st.ServerByID(srv.ID); err != store.ErrNotFound {
		t.Fatalf("a server that never pushed must be deleted outright: %v", err)
	}
}

// Deleting drops the live samples too: SQLite reuses row ids, so a future
// server could otherwise inherit a dead host's live chart.
func TestDeleteForgetsTheLiveSamples(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	pushed(t, a, srv)

	deleteServer(t, a, srv.ID, "?force=true")

	if points := a.live.Series(srv.ID, "cpu.usage"); len(points) != 0 {
		t.Fatalf("live samples survived the delete: %+v", points)
	}
}

func TestConfirmedUninstallForgetsTheLiveSamples(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	pushed(t, a, srv)
	deleteServer(t, a, srv.ID, "")

	postUninstalled(t, a, srv.Token)

	if points := a.live.Series(srv.ID, "cpu.usage"); len(points) != 0 {
		t.Fatalf("live samples survived the confirmed removal: %+v", points)
	}
}

// The panel has to be able to show that a host is on its way out, and why it
// is stuck when it is.
func TestListServersProjectsTheUninstallingState(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	pushed(t, a, srv)
	deleteServer(t, a, srv.ID, "")
	clearPushLimit(a)

	postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: "web-1", UninstallError: "uninstaller not found",
		Samples: map[string]float64{},
	})

	w := adminReq(t, a.Handler(), http.MethodGet, "/api/servers", "")
	var body struct {
		Data []struct {
			Status               string `json:"status"`
			UninstallError       string `json:"uninstall_error"`
			UninstallRequestedAt int64  `json:"uninstall_requested_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("fleet = %s", w.Body.String())
	}
	row := body.Data[0]
	if row.Status != "uninstalling" {
		t.Fatalf("status = %q, want uninstalling", row.Status)
	}
	if row.UninstallError != "uninstaller not found" {
		t.Fatalf("uninstall_error = %q", row.UninstallError)
	}
	if row.UninstallRequestedAt == 0 {
		t.Fatal("uninstall_requested_at not exposed")
	}
}
