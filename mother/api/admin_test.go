package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

func adminReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-API-Key", "adminkey")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func newRequest(method, path, body string) *http.Request {
	return httptest.NewRequest(method, path, strings.NewReader(body))
}

func recorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

func TestAdminRequiresAPIKey(t *testing.T) {
	a, _ := setup(t)
	r := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAddServerReturnsInstallCommand(t *testing.T) {
	a, _ := setup(t)
	w := adminReq(t, a.Handler(), http.MethodPost, "/api/servers", `{"name":"DB_Sunucusu"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var env envelope
	json.Unmarshal(w.Body.Bytes(), &env)
	var data struct {
		Server struct {
			ID         int64    `json:"id"`
			Name       string   `json:"name"`
			Token      string   `json:"token"`
			Collectors []string `json:"collectors"`
		} `json:"server"`
		InstallCommand string `json:"install_command"`
	}
	json.Unmarshal(env.Data, &data)
	if data.Server.Token == "" || data.Server.Name != "DB_Sunucusu" {
		t.Fatalf("server payload must use lowercase keys: %s", env.Data)
	}
	if !strings.Contains(data.InstallCommand, "/install/"+data.Server.Token+".sh") {
		t.Fatalf("install command: %q", data.InstallCommand)
	}
}

func TestAddServerRejectsInvalidNameWith400(t *testing.T) {
	a, _ := setup(t)
	w := adminReq(t, a.Handler(), http.MethodPost, "/api/servers", `{"name":"evil; x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body)
	}
}

func TestServerStatusPendingOnlineDown(t *testing.T) {
	a, st := setup(t)
	st.AddServer("never-pushed")
	fresh, _ := st.AddServer("fresh")
	stale, _ := st.AddServer("stale")

	postIngest(t, a.Handler(), fresh.Token, protocol.IngestRequest{Server: "fresh", Samples: map[string]float64{"cpu.usage": 1}})
	// stale pushed long ago: threshold(3) × interval(10) = 30s window
	st.TouchServer(stale.ID, store.Heartbeat{AgentVersion: "1.0.0"}, 1)

	w := adminReq(t, a.Handler(), http.MethodGet, "/api/servers", "")
	var env envelope
	json.Unmarshal(w.Body.Bytes(), &env)
	var list []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	json.Unmarshal(env.Data, &list)

	byName := map[string]string{}
	for _, s := range list {
		byName[s.Name] = s.Status
	}
	if byName["never-pushed"] != "pending" || byName["fresh"] != "online" || byName["stale"] != "down" {
		t.Fatalf("statuses: %v", byName)
	}
}

func TestSettingsUpdateAffectsIngestResponse(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")

	adminReq(t, a.Handler(), http.MethodPut, "/api/settings",
		`{"interval":30,"heartbeat_miss_threshold":3,"retention_raw_hours":48,"retention_1m_days":15,"retention_1h_days":75}`)

	w := postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{Server: "web-1", Samples: map[string]float64{}})
	var resp protocol.IngestResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Interval != 30 {
		t.Fatalf("settings not applied to ingest: %+v", resp)
	}
}

func TestUpdateCollectorsAndDeleteHistory(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("cf-1")

	w := adminReq(t, a.Handler(), http.MethodPut, fmt.Sprintf("/api/servers/%d/collectors", srv.ID),
		`{"collectors":["cpu","memory","uptime","disk","centrifugo"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("collectors update: %d %s", w.Code, w.Body)
	}
	got, _ := st.ServerByToken(srv.Token)
	if len(got.Collectors) != 5 || got.Collectors[4] != "centrifugo" {
		t.Fatalf("collectors: %v", got.Collectors)
	}

	st.ApplySamples(srv.ID, 1700000000, map[string]float64{"cpu.usage": 1})
	w = adminReq(t, a.Handler(), http.MethodDelete,
		fmt.Sprintf("/api/history?server_id=%d&from=0&to=2000000000", srv.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete history: %d", w.Code)
	}
}

func TestDeleteHistoryRequiresExplicitServerID(t *testing.T) {
	a, _ := setup(t)
	w := adminReq(t, a.Handler(), http.MethodDelete, "/api/history?from=0&to=100", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing server_id must 400, got %d", w.Code)
	}
	w = adminReq(t, a.Handler(), http.MethodDelete, "/api/history?server_id=abc&from=0&to=100", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed server_id must 400, got %d", w.Code)
	}
	w = adminReq(t, a.Handler(), http.MethodDelete, "/api/history?server_id=0&from=0&to=100", "")
	if w.Code != http.StatusOK {
		t.Fatalf("explicit server_id=0 (all) must succeed, got %d", w.Code)
	}
}

func TestSettingsRejectSubRateLimitInterval(t *testing.T) {
	a, _ := setup(t)
	w := adminReq(t, a.Handler(), http.MethodPut, "/api/settings",
		`{"interval":1,"heartbeat_miss_threshold":3,"retention_raw_hours":48,"retention_1m_days":15,"retention_1h_days":75}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("interval=1 must 400 (below 2s rate-limit gap), got %d", w.Code)
	}
}

// The panel warns about collectors the agent cannot run, so the capability
// set has to reach it through the server list.
func TestListServersExposesCapabilities(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("db-1")
	if err := st.SetCapabilities(srv.ID, []string{"cpu", "memory"}); err != nil {
		t.Fatal(err)
	}

	w := adminReq(t, a.Handler(), http.MethodGet, "/api/servers", "")

	var body struct {
		Data []struct {
			Capabilities []string `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || len(body.Data[0].Capabilities) != 2 {
		t.Fatalf("capabilities not exposed: %s", w.Body.String())
	}
}

// Enabling a collector the agent cannot run is allowed on purpose: an
// operator may prepare the selection before configuring the agent. The panel
// warns; the mother does not block.
func TestSetCollectorsAcceptsCollectorOutsideCapabilities(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("db-1")
	st.SetCapabilities(srv.ID, []string{"cpu", "memory"})

	w := adminReq(t, a.Handler(), http.MethodPut,
		fmt.Sprintf("/api/servers/%d/collectors", srv.ID),
		`{"collectors":["cpu","dragonfly"]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got, _ := st.ServerByToken(srv.Token)
	if len(got.Collectors) != 2 || got.Collectors[1] != "dragonfly" {
		t.Fatalf("collectors = %v", got.Collectors)
	}
}

// A settings PUT that omits a retention field must be rejected, not stored as
// zero. Storing zero makes the next retention sweep compute a cutoff of
// `now - 0` and delete every row in that tier for every server — fifteen days
// of 1-minute history destroyed by a partial payload. The panel always sends
// the full key set, so the exposure is any scripted or direct caller.
func TestSettingsRejectPartialPayload(t *testing.T) {
	cases := map[string]string{
		"missing retention_1m_days": `{"interval":10,"heartbeat_miss_threshold":3,"retention_raw_hours":48,"retention_1h_days":75}`,
		"missing retention_1h_days": `{"interval":10,"heartbeat_miss_threshold":3,"retention_raw_hours":48,"retention_1m_days":15}`,
		"missing interval":          `{"heartbeat_miss_threshold":3,"retention_raw_hours":48,"retention_1m_days":15,"retention_1h_days":75}`,
		"empty object":              `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			a, st := setup(t)
			before, _ := st.GetSettings()

			w := adminReq(t, a.Handler(), http.MethodPut, "/api/settings", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("partial settings must 400, got %d %s", w.Code, w.Body)
			}
			if after, _ := st.GetSettings(); after != before {
				t.Fatalf("rejected payload must not be stored: before %+v after %+v", before, after)
			}
		})
	}
}

// Retention values are what stand between the stored history and a DELETE, so
// a zero or negative value has to be refused at the boundary rather than
// silently becoming "delete everything".
func TestSettingsRejectNonPositiveRetention(t *testing.T) {
	for _, body := range []string{
		`{"interval":10,"heartbeat_miss_threshold":3,"retention_raw_hours":48,"retention_1m_days":-1,"retention_1h_days":75}`,
		`{"interval":10,"heartbeat_miss_threshold":3,"retention_raw_hours":48,"retention_1m_days":15,"retention_1h_days":0}`,
	} {
		w := func() *httptest.ResponseRecorder {
			a, _ := setup(t)
			return adminReq(t, a.Handler(), http.MethodPut, "/api/settings", body)
		}()
		if w.Code != http.StatusBadRequest {
			t.Fatalf("non-positive retention must 400, got %d for %s", w.Code, body)
		}
	}
}

// A complete, valid payload must still round-trip — the guard above must not
// be so strict that the panel's own save stops working.
func TestSettingsAcceptCompletePayload(t *testing.T) {
	a, st := setup(t)
	w := adminReq(t, a.Handler(), http.MethodPut, "/api/settings",
		`{"interval":20,"heartbeat_miss_threshold":4,"retention_raw_hours":24,"retention_1m_days":7,"retention_1h_days":90}`)
	if w.Code != http.StatusOK {
		t.Fatalf("complete settings must succeed, got %d %s", w.Code, w.Body)
	}
	got, _ := st.GetSettings()
	want := store.Settings{Interval: 20, HeartbeatMissThreshold: 4,
		Retention1mDays: 7, Retention1hDays: 90}
	if got != want {
		t.Fatalf("stored %+v want %+v", got, want)
	}
}
