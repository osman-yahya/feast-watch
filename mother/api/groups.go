package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/store"
)

func (a *API) registerGroups(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/groups", a.requireAPIKey(a.handleListGroups))
	mux.HandleFunc("POST /api/groups", a.requireAPIKey(a.handleCreateGroup))
	mux.HandleFunc("PATCH /api/groups/{id}", a.requireAPIKey(a.handleRenameGroup))
	mux.HandleFunc("DELETE /api/groups/{id}", a.requireAPIKey(a.handleDeleteGroup))
	mux.HandleFunc("PUT /api/groups/{id}/servers", a.requireAPIKey(a.handleSetGroupServers))
	mux.HandleFunc("PUT /api/groups/{id}/version", a.requireAPIKey(a.handleSetGroupVersion))
}

// groupRef is how a group appears attached to a server row: enough to render
// and filter by, without repeating the member count on every server.
type groupRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// groupRolloutResult reports a bulk version target.
//
// The split is between two different kinds of fault. A bad VERSION —
// unpublished, or the moving alias — is a property of the request and fails it
// outright with a 400. A missing build for one host's PLATFORM is a property of
// that host, so it is skipped and named while the rest proceed: one darwin
// laptop in a group must not permanently block a rollout across forty Linux
// servers.
type groupRolloutResult struct {
	Version string     `json:"version"`
	Applied []groupRef `json:"applied"`
	Skipped []skipped  `json:"skipped"`
}

type skipped struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

func groupID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "invalid id")
		return 0, false
	}
	return id, true
}

// writeGroupErr maps the store's sentinels onto status codes. A name collision
// and a missing group are the caller's to fix; anything else is ours.
func writeGroupErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, nil, "group not found")
	case errors.Is(err, store.ErrDuplicateGroupName):
		writeJSON(w, http.StatusConflict, nil, err.Error())
	case errors.Is(err, store.ErrInvalidGroupName):
		writeJSON(w, http.StatusBadRequest, nil, err.Error())
	default:
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
	}
}

func (a *API) handleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := a.st.ListGroups()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, groups, "")
}

func (a *API) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "name is required")
		return
	}
	g, err := a.st.CreateGroup(in.Name)
	if err != nil {
		writeGroupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g, "")
}

func (a *API) handleRenameGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := groupID(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "name is required")
		return
	}
	if err := a.st.RenameGroup(id, in.Name); err != nil {
		writeGroupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nil, "")
}

func (a *API) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := groupID(w, r)
	if !ok {
		return
	}
	if err := a.st.DeleteGroup(id); err != nil {
		writeGroupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nil, "")
}

func (a *API) handleSetGroupServers(w http.ResponseWriter, r *http.Request) {
	id, ok := groupID(w, r)
	if !ok {
		return
	}
	var in struct {
		ServerIDs []int64 `json:"server_ids"`
	}
	// An absent or null list is not the same as an empty one, but both mean
	// "no members" here, and emptying a group has to be expressible.
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "server_ids is required")
		return
	}
	if err := a.st.SetGroupServers(id, in.ServerIDs); err != nil {
		writeGroupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nil, "")
}

func (a *API) handleSetGroupVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := groupID(w, r)
	if !ok {
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

	members, err := a.groupMembers(id)
	if err != nil {
		writeGroupErr(w, err)
		return
	}

	// One snapshot for the whole group: re-reading per member would let a
	// refresh land mid-loop and target half the group against one list and
	// half against another.
	builds := a.releases.Snapshot().Builds

	result := groupRolloutResult{Version: in.Version, Applied: []groupRef{}, Skipped: []skipped{}}
	for _, srv := range members {
		// Clearing always applies: cancelling a rollout must reach every
		// member regardless of platform.
		if in.Version != "" {
			if msg := rejectVersion(srv, in.Version, builds); msg != "" {
				if versionLevelFault(srv, in.Version, builds) {
					writeJSON(w, http.StatusBadRequest, nil, msg)
					return
				}
				result.Skipped = append(result.Skipped, skipped{ID: srv.ID, Name: srv.Name, Reason: msg})
				continue
			}
		}
		if err := a.st.SetDesiredVersion(srv.ID, in.Version); err != nil {
			result.Skipped = append(result.Skipped, skipped{ID: srv.ID, Name: srv.Name, Reason: "storage failure"})
			continue
		}
		result.Applied = append(result.Applied, groupRef{ID: srv.ID, Name: srv.Name})
	}

	// Always 200, never success:false — writes landed. The envelope has no
	// partial state, so the split lives inside the data.
	writeJSON(w, http.StatusOK, result, "")
}

// versionLevelFault reports whether the reason this server was rejected is a
// property of the version rather than of the server, in which case no member
// could ever accept it and the whole request should fail.
func versionLevelFault(srv store.Server, version string, builds []release.Build) bool {
	if version == latestAlias {
		return true
	}
	for _, b := range builds {
		if b.Version == version {
			return false // the version exists; the fault was this platform
		}
	}
	return true
}

// groupMembers returns the group's servers, distinguishing an unknown group
// from an empty one.
func (a *API) groupMembers(id int64) ([]store.Server, error) {
	groups, err := a.st.ListGroups()
	if err != nil {
		return nil, err
	}
	found := false
	for _, g := range groups {
		if g.ID == id {
			found = true
			break
		}
	}
	if !found {
		return nil, store.ErrNotFound
	}
	return a.st.ServersInGroup(id)
}
