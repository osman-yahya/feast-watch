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

	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/store"
)

type API struct {
	st     *store.Store
	apiKey string
	// releases is the mother's view of what agent builds exist. The mother
	// stores no binaries and serves none: agents download from the public
	// GitHub release, and this is only the index a rollout target is checked
	// against.
	releases *release.Cache
	// publicURL is the base URL agents reach the mother on, scheme included,
	// e.g. "http://10.0.0.1:8443". It is a whole URL rather than a host:port
	// because the mother no longer decides the scheme: it serves plain HTTP
	// and may sit behind something that terminates TLS at another host, port
	// or path prefix. See mother.PublicURL.
	publicURL string

	mu       sync.Mutex
	lastPush map[int64]time.Time // per-server rate-limit state
}

func New(st *store.Store, apiKey string, releases *release.Cache) *API {
	return &API{
		st: st, apiKey: apiKey, releases: releases,
		// Plain HTTP by default and by construction. There is no setter that
		// can raise this to https on its own — the whole URL is supplied or it
		// is not, so a half-configured mother cannot hand agents a scheme it
		// does not serve.
		publicURL: "http://127.0.0.1:8443",
		lastPush:  map[int64]time.Time{},
	}
}

// SetPublicURL sets the base URL agents are told to reach the mother on.
// Validate it with mother.PublicURL before calling.
func (a *API) SetPublicURL(u string) { a.publicURL = u }

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ingest", a.handleIngest)
	a.registerAdmin(mux) // Task 11
	a.registerSettings(mux)
	a.registerGroups(mux)
	a.registerChart(mux)   // Task 12
	a.registerInstall(mux) // Task 13
	a.registerVersions(mux)
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
