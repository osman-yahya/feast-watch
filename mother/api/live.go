package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/osman-yahya/feast-watch/mother/live"
)

// maxLiveMetrics bounds one live read. The panel asks for the handful of
// series a chart actually draws; a caller asking for hundreds would turn a
// poll that runs every few seconds into a full copy of the store.
const maxLiveMetrics = 8

// liveResponse is what GET /api/live returns.
//
// WindowSeconds travels with the data because the window is operator
// configurable: without it the panel would have to guess how far back an
// empty tail means "nothing was pushed" rather than "we do not keep that far".
type liveResponse struct {
	WindowSeconds int64                   `json:"window_seconds"`
	Series        map[string][]live.Point `json:"series"`
}

func (a *API) registerLive(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/live", a.requireAPIKey(a.handleLive))
}

// handleLive serves the in-RAM tail for one server: the samples as the agent
// pushed them, at the cadence it pushed them, for the last configured window.
//
// It reads nothing from SQLite. That is the point — this is the resolution the
// rollups deliberately do not keep (see mother/live), and serving it from
// memory means a panel polling every few seconds never touches the single
// write connection the ingest path depends on.
func (a *API) handleLive(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serverID, err := strconv.ParseInt(q.Get("server_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "server_id is required")
		return
	}
	metrics, msg := parseLiveMetrics(q.Get("metric"))
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, nil, msg)
		return
	}

	out := liveResponse{
		WindowSeconds: int64(a.live.Window().Seconds()),
		Series:        make(map[string][]live.Point, len(metrics)),
	}
	for _, m := range metrics {
		points := a.live.Series(serverID, m)
		if points == nil {
			// An explicit empty list, not null: every requested metric is
			// answered, so the panel can map over the response without a
			// per-metric guard. A server with that collector switched off and
			// a server that has never pushed read the same way.
			points = []live.Point{}
		}
		out.Series[m] = points
	}
	writeJSON(w, http.StatusOK, out, "")
}

// parseLiveMetrics splits and validates the comma-separated metric list,
// returning the names or the message explaining why the list is unusable.
func parseLiveMetrics(raw string) ([]string, string) {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, "metric is required"
	}
	if len(out) > maxLiveMetrics {
		return nil, "too many metrics; at most " + strconv.Itoa(maxLiveMetrics) + " per request"
	}
	return out, ""
}
