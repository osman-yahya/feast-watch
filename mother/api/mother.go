package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/store"
)

// MotherUpdateTarget is what the API needs to know about the process it is
// retargeting: whether this deployment can actually promote a staged binary,
// and which platform's build it would need.
//
// Narrow on purpose. The API describes and arms the update; it never drives it.
type MotherUpdateTarget interface {
	Supported() bool
	Platform() string
}

// SetMotherUpdate wires the updater in. Left unset — in tests, and in any build
// that never constructs one — the mother reports `unsupported` and refuses
// targets, which is the honest answer rather than a nil dereference.
func (a *API) SetMotherUpdate(t MotherUpdateTarget) { a.motherUpdate = t }

func (a *API) registerMother(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/mother/version", a.requireAPIKey(a.handleSetMotherVersion))
}

func (a *API) motherUpdateSupported() bool {
	return a.motherUpdate != nil && a.motherUpdate.Supported()
}

func (a *API) motherPlatform() string {
	if a.motherUpdate == nil {
		return ""
	}
	return a.motherUpdate.Platform()
}

func (a *API) handleSetMotherVersion(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "malformed payload")
		return
	}
	in.Version = strings.TrimSpace(in.Version)

	// Empty clears the target, and is deliberately not validated: cancelling
	// has to work even when the version being cancelled is one the release
	// index no longer offers, which is often exactly why it is being cancelled.
	if in.Version != "" {
		if !a.motherUpdateSupported() {
			writeJSON(w, http.StatusBadRequest, nil, "self-update is not available on this deployment")
			return
		}
		if msg := rejectMotherVersion(in.Version, a.motherPlatform(), a.releases.Snapshot().Mother); msg != "" {
			writeJSON(w, http.StatusBadRequest, nil, msg)
			return
		}
	}

	if err := a.st.SetMotherDesiredVersion(in.Version, time.Now().Unix()); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"desired_version": in.Version}, "")
}

// rejectMotherVersion returns why this version cannot be the mother's target,
// or "" when it can.
//
// It mirrors rejectVersion for agents — same rules, same wording — so
// "publishable" means one thing across the system. It takes the build list
// rather than reading it, keeping the rule a pure function.
func rejectMotherVersion(ver, platform string, builds []release.Build) string {
	if ver == latestAlias {
		return "latest is a moving alias, not a version; pick a concrete version"
	}
	for _, b := range builds {
		if b.Version != ver {
			continue
		}
		for _, p := range b.Platforms {
			if p == platform {
				return ""
			}
		}
		return "no " + ver + " mother build for " + platform
	}
	return "version " + ver + " has no published release"
}

// motherUpdateState is a projection of the stored row, computed the way
// updateState computes an agent's, so the panel never re-implements the rule.
//
// `unsupported` outranks everything: where nothing can be promoted, no other
// state is actionable, and rendering `idle` would offer a control that could
// only disappoint. `failed` is tested before `idle` — unlike the agent's
// projection — because giving up clears the target and keeps the error, so a
// failed mother update has no desired version left to tell it from idle.
func motherUpdateState(row store.MotherUpdate, running string, supported bool) string {
	if !supported {
		return "unsupported"
	}
	if row.Error != "" {
		return "failed"
	}
	if row.DesiredVersion == "" || row.DesiredVersion == running {
		return "idle"
	}
	return "pending"
}
