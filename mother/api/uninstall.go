package api

import (
	"log/slog"
	"net/http"
)

// registerUninstall exposes the agent-authenticated end of the two-phase
// removal. It sits on /v1 with ingest, not on /api, because it is spoken by an
// agent's own bearer token rather than by the backend's API key.
func (a *API) registerUninstall(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/uninstalled", a.handleUninstalled)
}

// handleUninstalled is the confirmation that closes a scheduled removal: the
// uninstaller calls it after everything is off the host, and the row is
// dropped.
//
// It is reported by the UNINSTALLER, not by the agent, on purpose. The
// uninstaller stops the agent's service as its first step, so an agent can
// only ever report "I am about to remove myself" — and if that attempt then
// failed, the mother would already have forgotten a host that is still running
// an agent it can no longer reach. The script reports last, when the removal
// is a fact.
//
// The token authenticates; it does not authorise. A server nobody scheduled
// gets a 409: an agent token is deployed on the host it monitors, so treating
// it as permission to delete a server row — and with it every metric ever
// collected from that host — would make a stolen token a history-erasing
// credential.
func (a *API) handleUninstalled(w http.ResponseWriter, r *http.Request) {
	srv, status := a.bearerServer(r)
	if status != 0 {
		msg := "unauthorized"
		if status == http.StatusInternalServerError {
			msg = "storage failure"
		}
		writeJSON(w, status, nil, msg)
		return
	}
	if srv.UninstallRequestedAt == 0 {
		writeJSON(w, http.StatusConflict, nil, "no uninstall was requested for this server")
		return
	}
	if err := a.deleteServer(srv.ID); err != nil {
		slog.Error("delete server after uninstall", "server", srv.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	slog.Info("agent uninstalled itself", "server", srv.Name, "id", srv.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"}, "")
}
