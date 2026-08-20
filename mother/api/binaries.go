package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/osman-yahya/feast-watch/mother/mirror"
	"github.com/osman-yahya/feast-watch/mother/release"
	sharedrelease "github.com/osman-yahya/feast-watch/shared/release"
)

// BinarySource is where the mother gets the bytes an agent downloads.
//
// Two things satisfy it and the difference is where the binary came from, not
// how it is served: mother/mirror fetches a published release and keeps it,
// mother/build compiles it here. The routes below cannot tell them apart, and
// neither can the agent.
type BinarySource interface {
	Ensure(version, asset string) (string, error)
}

// SetBinarySource gives the mother something to serve builds from. Unset, the
// download routes answer 404 — which is what a mother whose agents fetch from
// the release host themselves should say, rather than pretending to hold
// builds.
func (a *API) SetBinarySource(s BinarySource) { a.binaries = s }

// The routes deliberately mirror GitHub's own URL shape:
//
//	/releases/download/<tag>/<asset>
//	/releases/latest/download/<asset>
//
// The agent builds its download URL with shared/release.DownloadURL and the
// installer with LatestDownloadURL, neither of which knows the mother might be
// in the way. Matching the shape is what makes pointing RELEASE_BASE_URL at the
// mother the entire change on the agent side — no new protocol, no new client.
//
// Unauthenticated, deliberately. These are the same bytes the public GitHub
// release serves to anyone; the token they would be gated by is the one thing
// on the agent that IS secret, and sending it on every binary fetch would
// spread it further for no gain. The mother sits on a private network.
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
		http.Error(w, "this mother serves no binaries", http.StatusNotFound)
		return
	}

	binary := strings.TrimSuffix(asset, sharedrelease.ChecksumSuffix)
	if err := mirror.Verify(binary); err != nil {
		http.Error(w, "no such build", http.StatusNotFound)
		return
	}
	if !a.indexKnows(tag) {
		http.Error(w, "no such release", http.StatusNotFound)
		return
	}

	path, err := a.binaries.Ensure(tag, binary)
	if err != nil {
		// The mother could not get it from the release host. Say so plainly:
		// the agent asking has no other route to the binary, so this is the
		// end of the road for that rollout until it is fixed.
		slog.Error("mirroring a release asset", "tag", tag, "asset", binary, "err", err)
		http.Error(w, "could not fetch this build from the release host", http.StatusBadGateway)
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
