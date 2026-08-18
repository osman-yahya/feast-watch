package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

const (
	maxSamplesPerPush = 256
	minPushGap        = 2 * time.Second // rate limit: one push per token per 2s
)

// metricName is the charset a metric key may use. Keys become primary-key
// values in both rollup tiers, so an unbounded string from an agent is an
// unbounded key: validation belongs at this boundary, not at the writer.
var metricName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)

const maxMetricNameLen = 64

// invalidMetric returns the first unacceptable key, or "" when all are fine.
func invalidMetric(samples map[string]float64) string {
	for k := range samples {
		if len(k) > maxMetricNameLen || !metricName.MatchString(k) {
			return k
		}
	}
	return ""
}

func (a *API) handleIngest(w http.ResponseWriter, r *http.Request) {
	srv, status := a.bearerServer(r)
	if status != 0 {
		msg := `{"error":"unauthorized"}`
		if status == http.StatusInternalServerError {
			msg = `{"error":"storage failure"}`
		}
		http.Error(w, msg, status)
		return
	}
	var req protocol.IngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, `{"error":"malformed payload"}`, http.StatusBadRequest)
		return
	}
	if len(req.Samples) > maxSamplesPerPush {
		http.Error(w, `{"error":"too many samples"}`, http.StatusBadRequest)
		return
	}
	// Rate limit sits after validation so rejected payloads never consume the
	// slot; it protects the storage write path (one push per token per 2s).
	if !a.allowPush(srv.ID) {
		http.Error(w, `{"error":"pushing too fast"}`, http.StatusTooManyRequests)
		return
	}

	if bad := invalidMetric(req.Samples); bad != "" {
		slog.Warn("rejected metric name", "server", srv.Name, "metric", bad)
		http.Error(w, `{"error":"invalid metric name"}`, http.StatusBadRequest)
		return
	}

	now := time.Now().Unix()
	// The rollups are maintained incrementally, so a replayed push would be
	// counted twice with no raw tier left to recompute from. A push is only
	// folded in when its timestamp is strictly newer than the last one
	// recorded for this server; anything else is acknowledged and dropped,
	// because a non-200 would stop the agent reading the interval from the
	// body and leave it stuck at whatever it last knew.
	if now > srv.LastPush {
		if err := a.st.ApplySamples(srv.ID, now, req.Samples); err != nil {
			slog.Error("apply samples", "server", srv.Name, "err", err)
			http.Error(w, `{"error":"storage failure"}`, http.StatusInternalServerError)
			return
		}
	}
	if err := a.st.TouchServer(srv.ID, store.Heartbeat{
		AgentVersion: req.AgentVersion, Hostname: req.Hostname, IP: req.IP,
		OS: req.OS, Arch: req.Arch, UpdateError: req.UpdateError,
	}, now); err != nil {
		slog.Error("touch server", "server", srv.Name, "err", err)
	}
	// Only the first push carries capabilities; SetCapabilities ignores an
	// empty report so the steady-state pushes that follow do not erase them.
	if err := a.st.SetCapabilities(srv.ID, req.Capabilities); err != nil {
		slog.Error("set capabilities", "server", srv.Name, "err", err)
	}

	settings, err := a.st.GetSettings()
	if err != nil {
		slog.Error("get settings", "err", err)
		http.Error(w, `{"error":"storage failure"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// The rollout target comes from this server's row, not a fleet-wide
	// setting, so one host can be updated and observed before the rest.
	json.NewEncoder(w).Encode(protocol.IngestResponse{
		Collectors:     srv.Collectors,
		Interval:       settings.Interval,
		DesiredVersion: srv.DesiredVersion,
	})
}
