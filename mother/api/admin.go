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
	if srv.LastPush == 0 {
		return "pending"
	}
	if now-srv.LastPush > int64(s.HeartbeatMissThreshold*s.Interval) {
		return "down"
	}
	return "online"
}

func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := a.st.ListServers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	settings, err := a.st.GetSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	now := time.Now().Unix()
	views := make([]serverView, 0, len(servers))
	for _, s := range servers {
		views = append(views, serverView{
			ID: s.ID, Name: s.Name, Status: status(s, settings, now),
			Collectors: s.Collectors, Capabilities: s.Capabilities,
			Hostname: s.Hostname, IP: s.IP, OS: s.OS, Arch: s.Arch,
			AgentVersion: s.AgentVersion, LastPush: s.LastPush,
			DesiredVersion: s.DesiredVersion, UpdateState: updateState(s),
			UpdateError: s.UpdateError,
		})
	}
	writeJSON(w, http.StatusOK, views, "")
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
	if err := a.st.DeleteServer(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, nil, "")
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
// scheme must match what the mother actually serves, so the command the
// operator copies reaches the same endpoint the installed agent will use.
func InstallCommand(scheme, publicAddr, token string) string {
	return fmt.Sprintf("curl -sSLk %s://%s/install/%s.sh | sudo bash", scheme, publicAddr, token)
}

// installCommand renders the one-liner shown by the panel and the CLI.
func (a *API) installCommand(token string) string {
	return InstallCommand(a.scheme, a.publicAddr, token)
}
