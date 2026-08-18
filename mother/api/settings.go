package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/osman-yahya/feast-watch/mother/store"
)

// Bounds for the panel-configurable knobs. Every retention field feeds a
// `now - value` cutoff in store.EnforceRetention, so a zero or negative value
// is not a small misconfiguration — it is a DELETE of that whole tier for
// every server. They are validated here, at the boundary, rather than trusted
// from the caller.
const (
	// minInterval matches the ingest rate limit (minPushGap): a 1s interval
	// would make every other push a 429 and flap server status.
	minInterval = 2
	maxInterval = 3600

	minHeartbeatMiss = 1
	maxHeartbeatMiss = 100

	// maxRetentionRawHours / maxRetentionDays exist to catch a fat-fingered
	// value that would keep history effectively forever, not to express a
	// storage policy.
	maxRetentionRawHours = 24 * 365
	maxRetentionDays     = 3650
)

func (a *API) registerSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings", a.requireAPIKey(a.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", a.requireAPIKey(a.handlePutSettings))
}

// settingsPayload shadows store.Settings with pointers so an omitted key is
// distinguishable from an explicit zero. Decoding straight into store.Settings
// cannot tell them apart: a payload missing `retention_1m_days` would store 0
// and the next retention sweep would delete fifteen days of history for every
// server. The panel always sends the full key set, so this guards scripted and
// direct callers.
type settingsPayload struct {
	Interval               *int `json:"interval"`
	HeartbeatMissThreshold *int `json:"heartbeat_miss_threshold"`
	RetentionRawHours      *int `json:"retention_raw_hours"`
	Retention1mDays        *int `json:"retention_1m_days"`
	Retention1hDays        *int `json:"retention_1h_days"`
}

// bounds names the inclusive range each field must fall in.
type bounds struct {
	name     string
	value    *int
	min, max int
}

func (p settingsPayload) fields() []bounds {
	return []bounds{
		{"interval", p.Interval, minInterval, maxInterval},
		{"heartbeat_miss_threshold", p.HeartbeatMissThreshold, minHeartbeatMiss, maxHeartbeatMiss},
		{"retention_raw_hours", p.RetentionRawHours, 1, maxRetentionRawHours},
		{"retention_1m_days", p.Retention1mDays, 1, maxRetentionDays},
		{"retention_1h_days", p.Retention1hDays, 1, maxRetentionDays},
	}
}

// validate returns the settings to store, or the message explaining why the
// payload is unusable. Settings are saved as a whole, so a partial payload is
// rejected outright rather than merged onto the current values — a merge would
// make the same request mean different things depending on what is stored.
func (p settingsPayload) validate() (store.Settings, string) {
	for _, f := range p.fields() {
		if f.value == nil {
			return store.Settings{}, fmt.Sprintf("%s is required; send the complete settings object", f.name)
		}
		if *f.value < f.min || *f.value > f.max {
			return store.Settings{}, fmt.Sprintf("%s must be between %d and %d", f.name, f.min, f.max)
		}
	}
	return store.Settings{
		Interval:               *p.Interval,
		HeartbeatMissThreshold: *p.HeartbeatMissThreshold,
		RetentionRawHours:      *p.RetentionRawHours,
		Retention1mDays:        *p.Retention1mDays,
		Retention1hDays:        *p.Retention1hDays,
	}, ""
}

func (a *API) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	s, err := a.st.GetSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, s, "")
}

func (a *API) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in settingsPayload
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "malformed settings")
		return
	}
	settings, msg := in.validate()
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, nil, msg)
		return
	}
	if err := a.st.SaveSettings(settings); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, settings, "")
}
