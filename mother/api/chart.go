package api

import (
	"net/http"
	"strconv"
)

// maxChartPoints bounds every chart response — the frontend can never receive
// more, regardless of range. Raw `samples` are NEVER queried here (spec).
const maxChartPoints = 500

type ChartPoint struct {
	TS  int64   `json:"ts"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

func (a *API) registerChart(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/chart", a.requireAPIKey(a.handleChart))
}

func (a *API) handleChart(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serverID, err1 := strconv.ParseInt(q.Get("server_id"), 10, 64)
	from, err2 := strconv.ParseInt(q.Get("from"), 10, 64)
	to, err3 := strconv.ParseInt(q.Get("to"), 10, 64)
	interval, err4 := strconv.ParseInt(q.Get("interval"), 10, 64)
	metric := q.Get("metric")
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || metric == "" || to <= from {
		writeJSON(w, http.StatusBadRequest, nil, "server_id, metric, from, to, interval are required")
		return
	}
	if interval < 60 {
		interval = 60
	}
	if (to-from)/interval > maxChartPoints {
		writeJSON(w, http.StatusBadRequest, nil, "range/interval exceeds max points; increase interval")
		return
	}

	// Tier selection: sub-hour resolution comes from rollup_1m, else rollup_1h.
	table := "rollup_1h"
	if interval < 3600 {
		table = "rollup_1m"
	}

	rows, err := a.st.DB().Query(`
		SELECT (window_start/?)*? AS bucket,
		       MIN(min), MAX(max), SUM(avg*cnt)/SUM(cnt)
		FROM `+table+`
		WHERE server_id = ? AND metric = ? AND window_start BETWEEN ? AND ?
		GROUP BY bucket ORDER BY bucket`,
		interval, interval, serverID, metric, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	defer rows.Close()

	points := []ChartPoint{}
	for rows.Next() {
		var p ChartPoint
		if err := rows.Scan(&p.TS, &p.Min, &p.Max, &p.Avg); err != nil {
			writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
			return
		}
		points = append(points, p)
	}
	writeJSON(w, http.StatusOK, points, "")
}
