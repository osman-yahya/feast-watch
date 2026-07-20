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
	st        *store.Store
	apiKey    string
	downloads string // directory holding agent binaries + .sha256 files

	mu       sync.Mutex
	lastPush map[int64]time.Time // per-server rate-limit state
}

func New(st *store.Store, apiKey string, downloads string) *API {
	return &API{st: st, apiKey: apiKey, downloads: downloads, lastPush: map[int64]time.Time{}}
}

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
			http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
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

func (a *API) registerAdmin(mux *http.ServeMux)   {} // replaced in Task 11
func (a *API) registerChart(mux *http.ServeMux)   {} // replaced in Task 12
func (a *API) registerInstall(mux *http.ServeMux) {} // replaced in Task 13
