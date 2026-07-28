package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

func setup(t *testing.T) (*API, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, "adminkey", t.TempDir()), st
}

func postIngest(t *testing.T, h http.Handler, token string, req protocol.IngestRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestIngestStoresSamplesAndReturnsConfig(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")

	w := postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: "web-1", AgentVersion: "1.2.0", Hostname: "h", IP: "10.0.0.7", OS: "linux",
		Samples: map[string]float64{"cpu.usage": 34.2},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	var resp protocol.IngestResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Interval != 10 || len(resp.Collectors) != 4 {
		t.Fatalf("config response: %+v", resp)
	}

	list, _ := st.ListServers()
	if list[0].AgentVersion != "1.2.0" || list[0].LastPush == 0 {
		t.Fatalf("server not touched: %+v", list[0])
	}
}

func TestIngestRejectsBadToken(t *testing.T) {
	a, _ := setup(t)
	w := postIngest(t, a.Handler(), "tk_bogus", protocol.IngestRequest{Server: "x"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	w = postIngest(t, a.Handler(), "", protocol.IngestRequest{Server: "x"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: want 401, got %d", w.Code)
	}
}

func TestIngestValidatesPayload(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")

	r := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader([]byte("{not json")))
	r.Header.Set("Authorization", "Bearer "+srv.Token)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: want 400, got %d", w.Code)
	}

	big := map[string]float64{}
	for i := 0; i < 300; i++ {
		big[string(rune('a'+i%26))+string(rune('0'+i%10))+string(rune('A'+i%26))+string(rune(i))] = 1
	}
	w = postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{Server: "web-1", Samples: big})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized payload: want 400, got %d", w.Code)
	}
}

func TestIngestRateLimitsPerToken(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	req := protocol.IngestRequest{Server: "web-1", Samples: map[string]float64{"cpu.usage": 1}}

	if w := postIngest(t, a.Handler(), srv.Token, req); w.Code != http.StatusOK {
		t.Fatalf("first push: %d", w.Code)
	}
	if w := postIngest(t, a.Handler(), srv.Token, req); w.Code != http.StatusTooManyRequests {
		t.Fatalf("immediate second push: want 429, got %d", w.Code)
	}

	other, _ := st.AddServer("web-2")
	if w := postIngest(t, a.Handler(), other.Token,
		protocol.IngestRequest{Server: "web-2", Samples: map[string]float64{"cpu.usage": 1}}); w.Code != http.StatusOK {
		t.Fatalf("rate limit must be per token, got %d", w.Code)
	}
}

// The agent is the only party that knows which service collectors its host is
// configured for, so its report has to be persisted on arrival.
func TestIngestStoresReportedCapabilities(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("db-1")

	w := postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server:       "db-1",
		AgentVersion: "v1",
		Capabilities: []string{"cpu", "memory", "postgres"},
		Samples:      map[string]float64{"cpu.usage": 12},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}

	got, _ := st.ServerByToken(srv.Token)
	if len(got.Capabilities) != 3 || got.Capabilities[2] != "postgres" {
		t.Fatalf("capabilities = %v", got.Capabilities)
	}
}

// Capabilities ride along with the identity fields on the first push only;
// the steady-state pushes that follow must not wipe them.
func TestIngestKeepsCapabilitiesWhenPushOmitsThem(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("db-1")

	postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: "db-1", Capabilities: []string{"cpu", "dragonfly"},
		Samples: map[string]float64{"cpu.usage": 1},
	})
	// Second push carries no capabilities, as a real agent's would not.
	postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: "db-1", Samples: map[string]float64{"cpu.usage": 2},
	})

	got, _ := st.ServerByToken(srv.Token)
	if len(got.Capabilities) != 2 {
		t.Fatalf("steady-state push erased capabilities: %v", got.Capabilities)
	}
}
