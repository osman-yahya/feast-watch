package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/version"
)

// binaryPrefix is the filename stem every staged agent build shares; the rest
// of the name is "<version>-<goos>-<goarch>" (see bin/release.sh).
const binaryPrefix = "feast-watch-agent-"

// checksumSuffix marks the companion file the agent verifies before replacing
// itself. A build without one is unusable, so it is never offered.
const checksumSuffix = ".sha256"

// latestAlias is the moving pointer the install script downloads. It is never
// offered as a rollout target: an agent can never *be* "latest", so it would
// re-download and restart on every push, forever.
const latestAlias = "latest"

// knownPlatforms is the set of "<goos>-<goarch>" suffixes release builds use.
// Parsing against a known set (rather than splitting on the last two dashes)
// keeps versions that themselves contain dashes, like "v1.3.0-rc1", intact.
var knownPlatforms = []string{
	"linux-amd64", "linux-arm64",
	"darwin-amd64", "darwin-arm64",
	"windows-amd64", "windows-arm64",
}

type agentBuild struct {
	Version   string   `json:"version"`
	Platforms []string `json:"platforms"`
}

type versionView struct {
	MotherVersion string       `json:"mother_version"`
	Agents        []agentBuild `json:"agents"`
}

func (a *API) registerVersions(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/version", a.requireAPIKey(a.handleGetVersion))
	mux.HandleFunc("PUT /api/servers/{id}/version", a.requireAPIKey(a.handleSetServerVersion))
}

// platform renders the "<goos>-<goarch>" token for a server, or "" when the
// agent has not reported both halves yet.
func platform(srv store.Server) string {
	if srv.OS == "" || srv.Arch == "" {
		return ""
	}
	return srv.OS + "-" + srv.Arch
}

// availableBuilds lists the agent builds staged in the downloads directory,
// newest version first. A build counts only when both the binary and its
// .sha256 are present, because the agent refuses to install without the
// checksum — offering a half-staged build would produce a button that always
// fails.
func (a *API) availableBuilds() ([]agentBuild, error) {
	entries, err := os.ReadDir(a.downloads)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []agentBuild{}, nil
		}
		return nil, err
	}

	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			present[e.Name()] = true
		}
	}

	platformsByVersion := map[string][]string{}
	for name := range present {
		ver, plat, ok := parseBuildName(name)
		if !ok || ver == latestAlias {
			continue
		}
		if !present[name+checksumSuffix] {
			continue
		}
		platformsByVersion[ver] = append(platformsByVersion[ver], plat)
	}

	builds := make([]agentBuild, 0, len(platformsByVersion))
	for ver, plats := range platformsByVersion {
		sort.Strings(plats)
		builds = append(builds, agentBuild{Version: ver, Platforms: plats})
	}
	sort.Slice(builds, func(i, j int) bool {
		return naturalLess(builds[j].Version, builds[i].Version) // descending
	})
	return builds, nil
}

// parseBuildName splits "feast-watch-agent-v1.3.0-linux-amd64" into its
// version and platform. Checksum files and legacy names without an explicit
// GOOS are rejected: the former are companions, the latter are compatibility
// aliases for agents installed before builds were platform-explicit.
func parseBuildName(name string) (ver, plat string, ok bool) {
	rest, found := strings.CutPrefix(name, binaryPrefix)
	if !found || strings.HasSuffix(name, checksumSuffix) {
		return "", "", false
	}
	for _, p := range knownPlatforms {
		if v, cut := strings.CutSuffix(rest, "-"+p); cut && v != "" {
			return v, p, true
		}
	}
	return "", "", false
}

// naturalLess orders version strings so v1.9.0 sorts before v1.10.0, which a
// plain string comparison gets backwards. It compares digit runs numerically
// and everything else lexically; it is an ordering aid for the panel's
// dropdown, not a semver implementation.
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		an, arest := leadingChunk(a)
		bn, brest := leadingChunk(b)
		if an != bn {
			ai, aerr := strconv.Atoi(an)
			bi, berr := strconv.Atoi(bn)
			if aerr == nil && berr == nil {
				return ai < bi
			}
			return an < bn
		}
		a, b = arest, brest
	}
	return len(a) < len(b)
}

// leadingChunk splits off the leading run of digits, or the leading run of
// non-digits when the string does not start with one.
func leadingChunk(s string) (chunk, rest string) {
	digits := s[0] >= '0' && s[0] <= '9'
	for i := 0; i < len(s); i++ {
		if (s[i] >= '0' && s[i] <= '9') != digits {
			return s[:i], s[i:]
		}
	}
	return s, ""
}

func (a *API) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	builds, err := a.availableBuilds()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "cannot read agent builds")
		return
	}
	writeJSON(w, http.StatusOK, versionView{MotherVersion: version.Version, Agents: builds}, "")
}

func (a *API) handleSetServerVersion(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "invalid id")
		return
	}
	var in struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "malformed payload")
		return
	}
	in.Version = strings.TrimSpace(in.Version)

	srv, err := a.st.ServerByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, nil, "server not found")
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}

	// Empty clears the target, which is how an operator cancels a rollout that
	// has not landed yet.
	if in.Version != "" {
		if msg := a.rejectVersion(srv, in.Version); msg != "" {
			writeJSON(w, http.StatusBadRequest, nil, msg)
			return
		}
	}

	if err := a.st.SetDesiredVersion(id, in.Version); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"desired_version": in.Version}, "")
}

// rejectVersion returns the reason this version cannot be a rollout target for
// this server, or "" when it is valid. Validating here rather than trusting the
// panel matters: an unstaged version makes every agent push retry a download
// that 404s, with the failure visible only in the agent's own logs.
func (a *API) rejectVersion(srv store.Server, ver string) string {
	if ver == latestAlias || strings.HasPrefix(ver, latestAlias+"-") {
		return "latest is a moving alias, not a version; pick a concrete version"
	}
	builds, err := a.availableBuilds()
	if err != nil {
		return "cannot read agent builds"
	}
	for _, b := range builds {
		if b.Version != ver {
			continue
		}
		want := platform(srv)
		// An agent installed before builds carried a GOOS reports no arch, so
		// its platform cannot be checked. Requiring one would lock those hosts
		// out of the very update that teaches them to report it.
		if want == "" {
			return ""
		}
		for _, p := range b.Platforms {
			if p == want {
				return ""
			}
		}
		return "no " + ver + " build for " + want
	}
	return "version " + ver + " is not staged on the mother"
}
