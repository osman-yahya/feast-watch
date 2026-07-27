// Package api is the mother's HTTP surface: agent ingest, backend admin API,
// install script and binary distribution.
package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
)

type API struct {
	st         *store.Store
	apiKey     string
	downloads  string // directory holding agent binaries + .sha256 files
	publicAddr string // host:port agents reach the mother on, e.g. "10.0.0.1:8443"
	// scheme is the URL scheme the mother actually serves ("https" when it
	// holds a TLS certificate, "http" otherwise). Agents are told to reach it
	// over this scheme; hardcoding https would hand a plain-HTTP mother an
	// install script whose every push fails.
	scheme string
	// agentTLSSkipVerify makes the generated install script write
	// TLS_SKIP_VERIFY=true into agent.conf. Required when the mother presents
	// a self-signed certificate: install.sh is the only channel that reaches
	// the agent's TLS configuration, and a CA file path cannot be transferred.
	agentTLSSkipVerify bool

	mu       sync.Mutex
	lastPush map[int64]time.Time // per-server rate-limit state
}

func New(st *store.Store, apiKey string, downloads string) *API {
	return &API{
		st: st, apiKey: apiKey, downloads: downloads,
		publicAddr: "127.0.0.1:8443", scheme: "https",
		lastPush: map[int64]time.Time{},
	}
}

func (a *API) SetPublicAddr(addr string) { a.publicAddr = addr }

// SetScheme sets the URL scheme agents use to reach the mother. Anything other
// than "http" is treated as "https" so a misconfiguration can never downgrade
// a TLS-serving mother to plaintext.
func (a *API) SetScheme(scheme string) {
	if scheme == "http" {
		a.scheme = "http"
		return
	}
	a.scheme = "https"
}

func (a *API) SetAgentTLSSkipVerify(skip bool) { a.agentTLSSkipVerify = skip }

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ingest", a.handleIngest)
	a.registerAdmin(mux)   // Task 11
	a.registerChart(mux)   // Task 12
	a.registerInstall(mux) // Task 13
	return mux
}

// bearerServer authenticates an agent push by its per-server token.
// Returns (server, 0) on success, (empty, 401) if token not found, or (empty, 500) on storage error.
func (a *API) bearerServer(r *http.Request) (store.Server, int) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return store.Server{}, http.StatusUnauthorized
	}
	srv, err := a.st.ServerByToken(tok)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Server{}, http.StatusUnauthorized
		}
		slog.Error("token lookup failed", "err", err)
		return store.Server{}, http.StatusInternalServerError
	}
	return srv, 0
}

// requireAPIKey guards the backend-facing admin surface.
func (a *API) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != a.apiKey || a.apiKey == "" {
			writeJSON(w, http.StatusUnauthorized, nil, "invalid api key")
			return
		}
		next(w, r)
	}
}

// allowPush is a per-token rate limiter: at most one push per minPushGap.
func (a *API) allowPush(serverID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if last, ok := a.lastPush[serverID]; ok && now.Sub(last) < minPushGap {
		return false
	}
	a.lastPush[serverID] = now
	return true
}
