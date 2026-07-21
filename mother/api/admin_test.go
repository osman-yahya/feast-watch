package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	st.TouchServer(stale.ID, "1.0.0", "", "", "", 1)

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
		`{"interval":30,"heartbeat_miss_threshold":3,"retention_raw_hours":48,"retention_1m_days":15,"retention_1h_days":75,"desired_version":"v9.9.9"}`)

	w := postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{Server: "web-1", Samples: map[string]float64{}})
	var resp protocol.IngestResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Interval != 30 || resp.DesiredVersion != "v9.9.9" {
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

	st.InsertSamples(srv.ID, 1700000000, map[string]float64{"cpu.usage": 1})
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
