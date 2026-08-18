package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

type chartPoint struct {
	TS  int64   `json:"ts"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

func TestChartReadsRollupsAndGroups(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	base := int64(1700000000) - 1700000000%3600

	// two minutes of pushes, folded into the rollups as they arrive
	for i := int64(0); i < 12; i++ { // 12 × 10s across 2 minutes
		st.ApplySamples(srv.ID, base+i*10, map[string]float64{"cpu.usage": float64(10 + i)})
	}

	// interval=120 → grouped from rollup_1m into one 2-minute bucket
	w := adminReq(t, a.Handler(), http.MethodGet,
		fmt.Sprintf("/api/chart?server_id=%d&metric=cpu.usage&from=%d&to=%d&interval=120", srv.ID, base, base+300), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var env envelope
	json.Unmarshal(w.Body.Bytes(), &env)
	var pts []chartPoint
	json.Unmarshal(env.Data, &pts)
	if len(pts) != 1 {
		t.Fatalf("want 1 grouped bucket, got %d: %+v", len(pts), pts)
	}
	if pts[0].Min != 10 || pts[0].Max != 21 {
		t.Fatalf("bucket bounds: %+v", pts[0])
	}
}

func TestChartRejectsUnboundedRequests(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	// 75 days at 60s interval would be 108k points — must be rejected, not served
	w := adminReq(t, a.Handler(), http.MethodGet,
		fmt.Sprintf("/api/chart?server_id=%d&metric=cpu.usage&from=0&to=6480000&interval=60", srv.ID), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for unbounded request, got %d", w.Code)
	}
}

func TestChartValidatesParams(t *testing.T) {
	a, _ := setup(t)
	w := adminReq(t, a.Handler(), http.MethodGet, "/api/chart?metric=cpu.usage", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing params: want 400, got %d", w.Code)
	}
}
