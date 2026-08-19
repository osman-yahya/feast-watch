package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
)

func writeJSON(w http.ResponseWriter, status int, data any, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"success": errMsg == "", "data": data, "error": errMsg,
	})
}

func (a *API) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers", a.requireAPIKey(a.handleListServers))
	mux.HandleFunc("POST /api/servers", a.requireAPIKey(a.handleAddServer))
	mux.HandleFunc("DELETE /api/servers/{id}", a.requireAPIKey(a.handleDeleteServer))
	mux.HandleFunc("PUT /api/servers/{id}/collectors", a.requireAPIKey(a.handleSetCollectors))
	mux.HandleFunc("DELETE /api/history", a.requireAPIKey(a.handleDeleteHistory))
}

type serverView struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Collectors []string `json:"collectors"`
	// Capabilities is what the agent reported it can actually run. Empty
	// means it has not reported yet, which consumers must treat as "unknown"
	// rather than "supports nothing".
	Capabilities []string `json:"capabilities"`
	Hostname     string   `json:"hostname"`
	IP           string   `json:"ip"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	AgentVersion string   `json:"agent_version"`
	LastPush     int64    `json:"last_push"`
	// DesiredVersion is the rollout target set from the panel; UpdateState is
	// how far the agent has got with it, and UpdateError is why it stopped.
	DesiredVersion string `json:"desired_version"`
	UpdateState    string `json:"update_state"`
	UpdateError    string `json:"update_error"`
	// UninstallRequestedAt is when an operator deleted this server, 0 when
	// nobody has; UninstallError is why the agent has not managed to remove
	// itself yet. Both are what the panel's "kaldırılıyor" row is built from.
	UninstallRequestedAt int64  `json:"uninstall_requested_at"`
	UninstallError       string `json:"uninstall_error"`
	// Groups the server belongs to. Always a list, never null: the panel maps
	// over it directly.
	Groups []groupRef `json:"groups"`
	// Latest is the newest value of every metric still inside the live window,
	// and LatestTS when the newest of them arrived (0 when there is nothing).
	// Embedded here rather than fetched per server so the fleet table and the
	// group overview both cost one request, at whatever cadence they poll.
	// Always an object, never null.
	Latest   map[string]float64 `json:"latest"`
	LatestTS int64              `json:"latest_ts"`
}

// updateState derives the rollout state the panel renders. It is a projection
// of the two stored fields, so the panel never has to re-implement the rule.
func updateState(srv store.Server) string {
	if srv.DesiredVersion == "" || srv.DesiredVersion == srv.AgentVersion {
		return "idle"
	}
	if srv.UpdateError != "" {
		return "failed"
	}
	return "pending"
}

func status(srv store.Server, s store.Settings, now int64) string {
	// Being on the way out outranks every other state: the row exists only to
	// carry the removal, and showing it as "online" would invite an operator
	// to act on a host that is deleting itself.
	if srv.UninstallRequestedAt != 0 {
		return "uninstalling"
	}
	if srv.LastPush == 0 {
		return "pending"
	}
	if now-srv.LastPush > int64(s.HeartbeatMissThreshold*s.Interval) {
		return "down"
	}
	return "online"
}

func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := a.listServers(w, r)
	if err != nil {
		return // listServers already answered
	}
	settings, err := a.st.GetSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	// Read the whole mapping once rather than once per row.
	groupsByServer, err := a.st.GroupsByServer()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}

	now := time.Now().Unix()
	views := make([]serverView, 0, len(servers))
	for _, s := range servers {
		latest, latestTS := a.live.Latest(s.ID)
		if latest == nil {
			latest = map[string]float64{}
		}
		groups := make([]groupRef, 0, len(groupsByServer[s.ID]))
		for _, g := range groupsByServer[s.ID] {
			groups = append(groups, groupRef{ID: g.ID, Name: g.Name})
		}
		views = append(views, serverView{
			ID: s.ID, Name: s.Name, Status: status(s, settings, now),
			Collectors: s.Collectors, Capabilities: s.Capabilities,
			Hostname: s.Hostname, IP: s.IP, OS: s.OS, Arch: s.Arch,
			AgentVersion: s.AgentVersion, LastPush: s.LastPush,
			DesiredVersion: s.DesiredVersion, UpdateState: updateState(s),
			UpdateError: s.UpdateError, Groups: groups,
			UninstallRequestedAt: s.UninstallRequestedAt,
			UninstallError:       s.UninstallError,
			Latest:               latest, LatestTS: latestTS,
		})
	}
	writeJSON(w, http.StatusOK, views, "")
}

// listServers reads the fleet, narrowed to one group when ?group_id= is given.
// It answers the request itself on failure and returns the error so the caller
// stops.
func (a *API) listServers(w http.ResponseWriter, r *http.Request) ([]store.Server, error) {
	raw := r.URL.Query().Get("group_id")
	if raw == "" {
		servers, err := a.st.ListServers()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		}
		return servers, err
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "invalid group_id")
		return nil, err
	}
	servers, err := a.st.ServersInGroup(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
	}
	return servers, err
}

func (a *API) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		writeJSON(w, http.StatusBadRequest, nil, "name is required")
		return
	}
	srv, err := a.st.AddServer(in.Name)
	if errors.Is(err, store.ErrInvalidName) {
		writeJSON(w, http.StatusBadRequest, nil, err.Error())
		return
	}
	if err != nil {
		writeJSON(w, http.StatusConflict, nil, "server name already exists")
		return
	}
	// Explicit lowercase keys — must match the casing of GET /api/servers so
	// consumers see one convention for "a server" across the surface.
	created := struct {
		ID         int64    `json:"id"`
		Name       string   `json:"name"`
		Token      string   `json:"token"`
		Collectors []string `json:"collectors"`
	}{ID: srv.ID, Name: srv.Name, Token: srv.Token, Collectors: srv.Collectors}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":          created,
		"install_command": a.installCommand(srv.Token),
	}, "")
}

func (a *API) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "invalid id")
		return
	}
	srv, err := a.st.ServerByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, nil, "server not found")
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}

	// Two-phase by default (see store/uninstall.go): the delete schedules the
	// agent's own removal and the row survives to carry it. Dropping the row
	// here instead would revoke the token the agent needs to be told anything,
	// leaving it installed, running and permanently unreachable.
	//
	// Two cases skip straight to the hard delete, because in both there is
	// nobody on the other end to tell:
	//   force=true — the operator says this host is never coming back;
	//   last_push == 0 — no agent ever reported, so nothing was ever installed
	//   that could report the removal.
	if r.URL.Query().Get("force") != "true" && srv.LastPush != 0 {
		if err := a.st.RequestUninstall(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "uninstalling"}, "")
		return
	}

	if err := a.deleteServer(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"}, "")
}

// deleteServer drops the row and everything keyed on the id, in SQLite and in
// memory both. Every path that removes a server goes through here: the live
// store is not covered by DeleteServer's transaction, and a forgotten Forget
// would hand a future server (ids are reused) a dead host's live chart.
func (a *API) deleteServer(id int64) error {
	if err := a.st.DeleteServer(id); err != nil {
		return err
	}
	a.live.Forget(id)
	return nil
}

func (a *API) handleSetCollectors(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "invalid id")
		return
	}
	var in struct {
		Collectors []string `json:"collectors"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Collectors) == 0 {
		writeJSON(w, http.StatusBadRequest, nil, "collectors list is required")
		return
	}
	if err := a.st.SetCollectors(id, in.Collectors); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, nil, "")
}

func (a *API) handleDeleteHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serverIDStr := q.Get("server_id")
	if serverIDStr == "" {
		writeJSON(w, http.StatusBadRequest, nil, "server_id is required (0 = all servers)")
		return
	}
	serverID, err := strconv.ParseInt(serverIDStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "server_id is required (0 = all servers)")
		return
	}
	from, err1 := strconv.ParseInt(q.Get("from"), 10, 64)
	to, err2 := strconv.ParseInt(q.Get("to"), 10, 64)
	if err1 != nil || err2 != nil || to < from {
		writeJSON(w, http.StatusBadRequest, nil, "from and to are required unix seconds")
		return
	}
	if err := a.st.DeleteHistory(serverID, from, to); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, nil, "")
}

// InstallCommand renders the one-liner install command shared by the panel,
// the admin API, and the `feast-watch generate` CLI (mother/generate.go).
// publicURL must be what the mother actually serves, so the command the
// operator copies reaches the same endpoint the installed agent will use.
//
// No -k: the mother serves plain HTTP, and anything terminating TLS in front
// of it is expected to present a certificate the host already trusts.
func InstallCommand(publicURL, token string) string {
	return fmt.Sprintf("curl -sSL %s/install/%s.sh | sudo bash", publicURL, token)
}

// installCommand renders the one-liner shown by the panel and the CLI.
func (a *API) installCommand(token string) string {
	return InstallCommand(a.publicURL, token)
}
