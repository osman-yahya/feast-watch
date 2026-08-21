package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/version"
)

// latestAlias is the moving pointer at the newest build the mother holds. The
// installer uses it, but it is never a rollout target: an agent can never *be*
// "latest", so it would re-download and restart on every push, forever.
const latestAlias = "latest"

type versionView struct {
	MotherVersion string          `json:"mother_version"`
	Agents        []release.Build `json:"agents"`
	// CheckedAt and Stale describe the index itself. An operator choosing a
	// rollout target should be able to see that the list is the last known
	// good answer rather than a current one.
	CheckedAt time.Time `json:"checked_at"`
	Stale     bool      `json:"stale"`

	// The mother's own rollout: what it could become, what it was told to
	// become, and how that is going. Added alongside the agent fields rather
	// than in a nested object so an older panel reading this payload is
	// unaffected.
	//
	// MotherPlatform is what a build must cover for a version to be
	// selectable, and is empty where self-update is unavailable at all.
	MotherBuilds         []release.Build `json:"mother_builds"`
	MotherPlatform       string          `json:"mother_platform"`
	MotherDesiredVersion string          `json:"mother_desired_version"`
	MotherUpdateState    string          `json:"mother_update_state"`
	MotherUpdateError    string          `json:"mother_update_error"`
}

func (a *API) registerVersions(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/version", a.requireAPIKey(a.handleGetVersion))
	mux.HandleFunc("PUT /api/servers/{id}/version", a.requireAPIKey(a.handleSetServerVersion))
}

// platform renders the "<goos>-<goarch>" token for a server, or "" when the
// agent has not reported both halves yet.
func platform(srv store.Server) string {
	if srv.OS == "" || srv.Arch == "" {
		return ""
	}
	return srv.OS + "-" + srv.Arch
}

func (a *API) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	snap := a.releases.Snapshot()
	row, err := a.st.MotherUpdate()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, versionView{
		MotherVersion: version.Version,
		Agents:        snap.Builds,
		CheckedAt:     snap.CheckedAt,
		Stale:         snap.Stale,

		MotherBuilds:         snap.Mother,
		MotherPlatform:       a.motherPlatform(),
		MotherDesiredVersion: row.DesiredVersion,
		MotherUpdateState:    motherUpdateState(row, version.Version, a.motherUpdateSupported()),
		MotherUpdateError:    row.Error,
	}, "")
}

func (a *API) handleSetServerVersion(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "invalid id")
		return
	}
	var in struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "malformed payload")
		return
	}
	in.Version = strings.TrimSpace(in.Version)

	srv, err := a.st.ServerByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, nil, "server not found")
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}

	// Empty clears the target, which is how an operator cancels a rollout that
	// has not landed yet.
	if in.Version != "" {
		if msg := rejectVersion(srv, in.Version, a.releases.Snapshot().Builds); msg != "" {
			writeJSON(w, http.StatusBadRequest, nil, msg)
			return
		}
	}

	if err := a.st.SetDesiredVersion(id, in.Version); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"desired_version": in.Version}, "")
}

// rejectVersion returns the reason this version cannot be a rollout target for
// this server, or "" when it is valid.
//
// It takes the build list rather than reading it, so a bulk operation over a
// group checks every member against one snapshot instead of re-reading per
// server — and so the rule itself stays a pure function.
//
// Validating here rather than trusting the caller matters: a version nobody
// built makes every agent push retry a download that 404s, with the failure
// visible only in the agent's own logs until it reports the error back.
func rejectVersion(srv store.Server, ver string, builds []release.Build) string {
	if ver == latestAlias {
		return "latest is a moving alias, not a version; pick a concrete version"
	}
	for _, b := range builds {
		if b.Version != ver {
			continue
		}
		want := platform(srv)
		// An agent that has not pushed yet reports no platform, so it cannot
		// be checked. Requiring one would lock a freshly added server out of
		// being given a target before it first reports.
		if want == "" {
			return ""
		}
		for _, p := range b.Platforms {
			if p == want {
				return ""
			}
		}
		return "no " + ver + " build for " + want
	}
	return "version " + ver + " has not been built on this mother"
}
