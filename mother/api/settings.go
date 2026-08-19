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

	// maxRetentionDays exists to catch a fat-fingered value that would keep
	// history effectively forever, not to express a storage policy.
	maxRetentionDays = 3650

	// The live window is a memory budget, not a retention policy: every
	// minute of it is held in the mother's RAM for every server. The ceiling
	// is what stops an operator from asking for an hour of 2-second samples
	// for the whole fleet on a box that also has to serve ingest.
	minLiveWindowMinutes = 1
	maxLiveWindowMinutes = 60
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
	Retention1mDays        *int `json:"retention_1m_days"`
	Retention1hDays        *int `json:"retention_1h_days"`

	// LiveWindowMinutes is OPTIONAL, unlike every field above. Omitting it
	// preserves the stored value instead of being rejected, for two reasons:
	// it gates no DELETE (nothing is persisted, so a wrong value costs memory
	// and nothing else), and every caller that predates it — including a
	// deployed panel — would otherwise start getting a 400 on save the moment
	// this mother ships.
	LiveWindowMinutes *int `json:"live_window_minutes"`

	// RetentionRawHours is accepted and ignored. There is no raw tier left for
	// it to bound, but an older panel or proxy still sends it, and rejecting
	// their payload would break settings entirely while they catch up. It is
	// deliberately not in the required set below.
	RetentionRawHours *int `json:"retention_raw_hours"`
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
		{"retention_1m_days", p.Retention1mDays, 1, maxRetentionDays},
		{"retention_1h_days", p.Retention1hDays, 1, maxRetentionDays},
	}
}

// validate returns the settings to store, or the message explaining why the
// payload is unusable. Settings are saved as a whole, so a partial payload is
// rejected outright rather than merged onto the current values — a merge would
// make the same request mean different things depending on what is stored.
//
// current is what is stored today, and is used for exactly one thing: keeping
// the live window when the payload omits it (see settingsPayload).
func (p settingsPayload) validate(current store.Settings) (store.Settings, string) {
	for _, f := range p.fields() {
		if f.value == nil {
			return store.Settings{}, fmt.Sprintf("%s is required; send the complete settings object", f.name)
		}
		if *f.value < f.min || *f.value > f.max {
			return store.Settings{}, fmt.Sprintf("%s must be between %d and %d", f.name, f.min, f.max)
		}
	}
	liveWindow := current.LiveWindowMinutes
	if p.LiveWindowMinutes != nil {
		liveWindow = *p.LiveWindowMinutes
		if liveWindow < minLiveWindowMinutes || liveWindow > maxLiveWindowMinutes {
			return store.Settings{}, fmt.Sprintf("live_window_minutes must be between %d and %d",
				minLiveWindowMinutes, maxLiveWindowMinutes)
		}
	}
	return store.Settings{
		Interval:               *p.Interval,
		HeartbeatMissThreshold: *p.HeartbeatMissThreshold,
		Retention1mDays:        *p.Retention1mDays,
		Retention1hDays:        *p.Retention1hDays,
		LiveWindowMinutes:      liveWindow,
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
	current, err := a.st.GetSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	settings, msg := in.validate(current)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, nil, msg)
		return
	}
	if err := a.st.SaveSettings(settings); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	// The live window lives in memory, not in SQLite, so a save has to reach
	// the store that holds it or the change would only take effect at the next
	// restart.
	a.ApplySettings(settings)
	writeJSON(w, http.StatusOK, settings, "")
}
