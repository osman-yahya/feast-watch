package api

import (
	"bytes"
	_ "embed"
	"log/slog"
	"net/http"
	"strings"
	"text/template"
)

//go:embed install.sh.tmpl
var installTmplSrc string

// uninstallScript is served verbatim and is also what the installer writes to
// disk, so there is exactly one uninstaller and it cannot drift from itself.
//
//go:embed uninstall.sh
var uninstallScript string

// missingkey=error turns a field the handler forgot to supply into a render
// error instead of the literal "<no value>". That string in a shell script is
// a syntax error at best and a wrong URL at worst, on a script that is piped
// straight into `sudo bash` on a production host.
var installTmpl = template.Must(
	template.New("install").Option("missingkey=error").Parse(installTmplSrc))

// registerInstall exposes the per-token install script. The script it renders
// names one address — this mother — for the config, the uninstaller and the
// binary alike, because that is the only address the host it runs on can
// reach. The download routes it uses are registered in binaries.go.
func (a *API) registerInstall(mux *http.ServeMux) {
	mux.HandleFunc("GET /install/{token}", a.handleInstallScript)
	// Unauthenticated on purpose. It carries no secret, and requiring a token
	// would 404 the moment the operator deletes the server — which is exactly
	// when a host most needs cleaning up.
	mux.HandleFunc("GET /uninstall.sh", a.handleUninstallScript)
}

func (a *API) handleUninstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write([]byte(uninstallScript))
}

func (a *API) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(r.PathValue("token"), ".sh")
	srv, err := a.st.ServerByToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Rendered into a buffer first: writing straight to the response would
	// stream a partial script to a host that is piping it into `sudo bash`, so
	// a render failure part-way through would execute a prefix of the
	// installer rather than nothing at all.
	var buf bytes.Buffer
	if err := installTmpl.Execute(&buf, map[string]any{
		"MotherURL":  a.publicURL,
		"Token":      srv.Token,
		"ServerName": srv.Name,
	}); err != nil {
		slog.Error("render install script", "err", err)
		http.Error(w, "install script unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write(buf.Bytes())
}
