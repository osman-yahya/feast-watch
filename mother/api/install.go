package api

import (
	"bytes"
	_ "embed"
	"log/slog"
	"net/http"
	"strings"
	"text/template"

	"github.com/osman-yahya/feast-watch/shared/release"
)

//go:embed install.sh.tmpl
var installTmplSrc string

// missingkey=error turns a field the handler forgot to supply into a render
// error instead of the literal "<no value>". That string in a shell script is
// a syntax error at best and a wrong URL at worst, on a script that is piped
// straight into `sudo bash` on a production host.
var installTmpl = template.Must(
	template.New("install").Option("missingkey=error").Parse(installTmplSrc))

// registerInstall exposes the per-token install script. There is deliberately
// no binary download route: agents fetch builds from the public GitHub
// release, so the mother stores no binaries, serves no bytes, and a rollout
// cannot be blocked by a file nobody staged on it.
func (a *API) registerInstall(mux *http.ServeMux) {
	mux.HandleFunc("GET /install/{token}", a.handleInstallScript)
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
		"MotherURL":      a.publicURL,
		"Token":          srv.Token,
		"ServerName":     srv.Name,
		"ReleaseBaseURL": release.DefaultBaseURL,
	}); err != nil {
		slog.Error("render install script", "err", err)
		http.Error(w, "install script unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write(buf.Bytes())
}
