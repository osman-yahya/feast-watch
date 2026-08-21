package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/osman-yahya/feast-watch/mother/release"
	sharedrelease "github.com/osman-yahya/feast-watch/shared/release"
)

// BinarySource is where the mother gets the bytes an agent downloads:
// mother/build.Store, reading the catalogue this mother compiled into. It is an
// interface so these routes can be tested without a compiler on the machine
// running the tests, not because a second implementation is expected — the
// mother building what its fleet runs is the whole arrangement, not an option
// within it.
type BinarySource interface {
	Ensure(version, asset string) (string, error)
}

// SetBinarySource gives the mother its catalogue. Every mother has one; a nil
// source is a wiring mistake rather than a deployment choice, and the download
// routes answer 404 rather than panicking so the mistake shows up as an agent
// that cannot update instead of as a dead process.
func (a *API) SetBinarySource(s BinarySource) { a.binaries = s }

// The routes keep GitHub's URL shape:
//
//	/releases/download/<tag>/<asset>
//	/releases/latest/download/<asset>
//
// Nothing is on the other end of it any more — the bytes are compiled here —
// but the shape is what the agent (shared/release.DownloadURL) and the served
// installer (LatestDownloadURL) already build, and keeping it meant the agents
// needed no new protocol and no new client when the source of their binaries
// moved home.
//
// Unauthenticated, deliberately. The token that would gate them is the one
// thing on an agent that IS secret, and sending it on every binary fetch would
// spread it further to protect bytes that every agent on the fleet is running
// anyway. The mother sits on a private network, and so do they.
func (a *API) registerBinaries(mux *http.ServeMux) {
	mux.HandleFunc("GET /releases/download/{tag}/{asset}", a.handleDownload)
	mux.HandleFunc("GET /releases/latest/download/{asset}", a.handleDownloadLatest)
}

func (a *API) handleDownload(w http.ResponseWriter, r *http.Request) {
	a.serveAsset(w, r, r.PathValue("tag"), r.PathValue("asset"))
}

func (a *API) handleDownloadLatest(w http.ResponseWriter, r *http.Request) {
	asset := r.PathValue("asset")
	tag, ok := a.latestTagFor(asset)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.serveAsset(w, r, tag, asset)
}

// latestTagFor resolves the moving pointer against the release index, per
// family: the newest agent build and the newest mother build are not
// necessarily the same release, and answering with the wrong family's newest
// would hand out a tag that has no such asset.
func (a *API) latestTagFor(asset string) (string, bool) {
	kind, _, ok := sharedrelease.AssetKindOf(strings.TrimSuffix(asset, sharedrelease.ChecksumSuffix))
	if !ok {
		return "", false
	}
	snap := a.releases.Snapshot()
	builds := snap.Builds
	if kind == sharedrelease.KindMother {
		builds = snap.Mother
	}
	if len(builds) == 0 {
		return "", false
	}
	// The index is sorted newest first.
	return builds[0].Version, true
}

// serveAsset validates both halves of the request before either becomes a path.
//
// The tag and the asset name arrive from the network and are joined into a
// filesystem path, so neither may be taken on trust: the asset must be a name
// this project publishes, and the tag must be one the release index has
// actually seen. Together that is an allowlist rather than an escaping rule,
// which is the only kind of defence that stays correct when someone adds a new
// path separator to the language.
func (a *API) serveAsset(w http.ResponseWriter, r *http.Request, tag, asset string) {
	if a.binaries == nil {
		slog.Error("a download was asked for but this mother has no build catalogue")
		http.Error(w, "this mother serves no binaries", http.StatusNotFound)
		return
	}

	binary := strings.TrimSuffix(asset, sharedrelease.ChecksumSuffix)
	if _, _, ok := sharedrelease.AssetKindOf(binary); !ok {
		http.Error(w, "no such build", http.StatusNotFound)
		return
	}
	if !a.indexKnows(tag) {
		http.Error(w, "no such release", http.StatusNotFound)
		return
	}

	path, err := a.binaries.Ensure(tag, binary)
	if err != nil {
		// The index named a version this platform has no binary for. Say so
		// plainly: the agent asking has no other route to a binary, so this is
		// the end of the road for that rollout until somebody builds it here.
		slog.Error("serving a build from the catalogue", "tag", tag, "asset", binary, "err", err)
		http.Error(w, "this build is not in the catalogue", http.StatusNotFound)
		return
	}
	if asset != binary {
		path += sharedrelease.ChecksumSuffix
	}
	// ServeFile rather than a copy: it answers conditional and range requests,
	// so an agent whose 12MB transfer was cut short resumes instead of starting
	// over — which on the link these agents are on is the difference between a
	// rollout that lands and one that keeps not landing.
	http.ServeFile(w, r, path)
}

// indexKnows reports whether the release index has seen this tag in either
// family.
func (a *API) indexKnows(tag string) bool {
	snap := a.releases.Snapshot()
	for _, list := range [][]release.Build{snap.Builds, snap.Mother} {
		for _, b := range list {
			if b.Version == tag {
				return true
			}
		}
	}
	return false
}
