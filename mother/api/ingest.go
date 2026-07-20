package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/osman-yahya/feast-watch/shared/protocol"
)

const (
	maxSamplesPerPush = 256
	minPushGap        = 2 * time.Second // rate limit: one push per token per 2s
)

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

	now := time.Now().Unix()
	if err := a.st.InsertSamples(srv.ID, now, req.Samples); err != nil {
		slog.Error("insert samples", "server", srv.Name, "err", err)
		http.Error(w, `{"error":"storage failure"}`, http.StatusInternalServerError)
		return
	}
	if err := a.st.TouchServer(srv.ID, req.AgentVersion, req.Hostname, req.IP, req.OS, now); err != nil {
		slog.Error("touch server", "server", srv.Name, "err", err)
	}

	settings, err := a.st.GetSettings()
	if err != nil {
		slog.Error("get settings", "err", err)
		http.Error(w, `{"error":"storage failure"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(protocol.IngestResponse{
		Collectors:     srv.Collectors,
		Interval:       settings.Interval,
		DesiredVersion: settings.DesiredVersion,
	})
}
