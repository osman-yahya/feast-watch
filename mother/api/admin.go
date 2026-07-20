package api

import (
	"encoding/json"
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
	mux.HandleFunc("GET /api/settings", a.requireAPIKey(a.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", a.requireAPIKey(a.handlePutSettings))
	mux.HandleFunc("DELETE /api/history", a.requireAPIKey(a.handleDeleteHistory))
}

type serverView struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Collectors   []string `json:"collectors"`
	Hostname     string   `json:"hostname"`
	IP           string   `json:"ip"`
	OS           string   `json:"os"`
	AgentVersion string   `json:"agent_version"`
	LastPush     int64    `json:"last_push"`
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
			Collectors: s.Collectors, Hostname: s.Hostname, IP: s.IP, OS: s.OS,
			AgentVersion: s.AgentVersion, LastPush: s.LastPush,
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
	if err != nil {
		writeJSON(w, http.StatusConflict, nil, "server name already exists")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":          srv,
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

func (a *API) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	s, err := a.st.GetSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, s, "")
}

func (a *API) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in store.Settings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "malformed settings")
		return
	}
	if in.Interval < 1 || in.HeartbeatMissThreshold < 1 {
		writeJSON(w, http.StatusBadRequest, nil, "interval and threshold must be >= 1")
		return
	}
	if err := a.st.SaveSettings(in); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, in, "")
}

func (a *API) handleDeleteHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serverID, _ := strconv.ParseInt(q.Get("server_id"), 10, 64)
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

// installCommand renders the one-liner shown by the panel and the CLI.
func (a *API) installCommand(token string) string {
	return fmt.Sprintf("curl -sSLk https://%s/install/%s.sh | sudo bash", a.publicAddr, token)
}
