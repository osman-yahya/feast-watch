package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

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
//
// ServerTime is the mother's own clock at the moment it answered. Every point
// is stamped by that clock (ingest uses its own time.Now), so a reader slicing
// "the last five minutes" against the browser's clock slices the wrong window
// as soon as the two disagree — which, across a VPN to a host nobody has
// looked at the NTP state of, they eventually do. Sending the clock the
// timestamps came from is cheaper than making every caller correct for skew it
// cannot measure.
type liveResponse struct {
	WindowSeconds int64                   `json:"window_seconds"`
	ServerTime    int64                   `json:"server_time"`
	Series        map[string][]live.Point `json:"series"`
}

func (a *API) registerLive(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/live", a.requireAPIKey(a.handleLive))
}

// handleLive serves the in-RAM tail for one server: the samples as the agent
// pushed them, at the cadence it pushed them, for the last configured window.
//
// `since` narrows that to what arrived after a timestamp the caller already
// holds, which is what every poll after the first one wants: the window itself
// changes by a point or two between reads, and re-sending all of it is the
// same bytes over and over for as long as a tab is open.
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
	since, msg := parseLiveSince(q.Get("since"))
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, nil, msg)
		return
	}

	out := liveResponse{
		WindowSeconds: int64(a.live.Window().Seconds()),
		ServerTime:    time.Now().Unix(),
		Series:        make(map[string][]live.Point, len(metrics)),
	}
	for _, m := range metrics {
		points := a.live.SeriesSince(serverID, m, since)
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

// parseLiveSince reads the optional `since` bound: unix seconds the caller
// already holds everything up to, or 0 for "I hold nothing".
//
// Absent is 0, but malformed is an error rather than 0. Both would "work" —
// the reader would get the whole window back — but a caller that fat-fingers
// the parameter would then poll a full window forever and never be told,
// which is precisely the cost this parameter exists to remove. A negative
// value is refused for the same reason: it can only be a bug at the caller,
// since these are unix seconds.
func parseLiveSince(raw string) (int64, string) {
	if raw == "" {
		return 0, ""
	}
	since, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || since < 0 {
		return 0, "since must be a unix timestamp in seconds"
	}
	return since, ""
}
