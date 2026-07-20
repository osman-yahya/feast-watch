package api

import (
	_ "embed"
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
	installTmpl.Execute(w, map[string]string{
		"MotherURL":  "https://" + a.publicAddr,
		"Token":      srv.Token,
		"ServerName": srv.Name,
	})
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
