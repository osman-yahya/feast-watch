package api

import (
	_ "embed"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed install.sh.tmpl
var installTmplSrc string

var installTmpl = template.Must(template.New("install").Parse(installTmplSrc))

func (a *API) registerInstall(mux *http.ServeMux) {
	mux.HandleFunc("GET /install/{token}", a.handleInstallScript)
	mux.HandleFunc("GET /download/agent/{version}", a.handleDownload)
}

func (a *API) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(r.PathValue("token"), ".sh")
	srv, err := a.st.ServerByToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	if err := installTmpl.Execute(w, map[string]any{
		"MotherURL":     a.scheme + "://" + a.publicAddr,
		"Token":         srv.Token,
		"ServerName":    srv.Name,
		"TLSSkipVerify": a.agentTLSSkipVerify,
	}); err != nil {
		// Headers are already sent at this point; nothing left to do but log.
		slog.Error("render install script", "err", err)
	}
}

func (a *API) handleDownload(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	// filepath.Base strips any traversal attempt; names are flat in downloads/.
	name := filepath.Base("feast-watch-agent-" + version)
	if strings.Contains(version, "/") || strings.Contains(version, "..") {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, filepath.Join(a.downloads, name))
}
