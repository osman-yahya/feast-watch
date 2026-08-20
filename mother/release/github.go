// Package release keeps the mother's view of what builds exist — the agents'
// and its own.
//
// The mother no longer stores or serves binaries. It reads the published
// releases from the GitHub API and holds an immutable snapshot, so naming a
// rollout target is a check against what is actually downloadable rather than
// against a directory somebody remembered to stage.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	sharedrelease "github.com/osman-yahya/feast-watch/shared/release"
)

// maxBodySize caps the API response. The release list is small; anything this
// large is a wrong endpoint or an error page, not data.
const maxBodySize = 4 << 20

// perPage is GitHub's maximum. One page is far more history than a rollout
// dropdown can use, so pagination is deliberately not followed.
const perPage = 100

// Build is one downloadable version and the platforms it was built for. The
// family it belongs to is carried by which list it is in, not by a field: every
// consumer wants exactly one of them, and a Kind field would move the
// separation to each call site instead of settling it here.
type Build struct {
	Version   string   `json:"version"`
	Platforms []string `json:"platforms"`
}

// Client reads the release list from the GitHub API, using a conditional
// request so an unchanged list costs nothing against the rate limit.
type Client struct {
	apiBaseURL         string
	includePrereleases bool
	http               *http.Client
	etag               string
}

func NewClient(apiBaseURL string, includePrereleases bool) *Client {
	return &Client{
		apiBaseURL:         apiBaseURL,
		includePrereleases: includePrereleases,
		http:               &http.Client{Timeout: 15 * time.Second},
	}
}

type apiRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

// Fetch returns the current builds of both families, or notModified when the
// list is unchanged since the last call.
//
// Unauthenticated GitHub allows 60 requests an hour per IP and does not count
// a conditional request answered 304. Polling without the ETag would spend a
// large part of that budget on a list that changes a few times a month — and
// run out during exactly the incident where someone is trying to roll back.
func (c *Client) Fetch(ctx context.Context) (agents, mother []Build, notModified bool, err error) {
	url := c.apiBaseURL + "/repos/" + sharedrelease.Repo + "/releases?per_page=" + strconv.Itoa(perPage)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// GitHub rejects a request without a User-Agent outright.
	req.Header.Set("User-Agent", "feast-watch-mother")
	if c.etag != "" {
		req.Header.Set("If-None-Match", c.etag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, nil, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, false, fmt.Errorf("github releases: %s", resp.Status)
	}

	var releases []apiRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodySize)).Decode(&releases); err != nil {
		return nil, nil, false, fmt.Errorf("decode github releases: %w", err)
	}
	c.etag = resp.Header.Get("ETag")
	agents, mother = buildsFrom(releases, c.includePrereleases)
	return agents, mother, false, nil
}

// buildsFrom keeps only what could actually be installed: a published release
// with, for at least one platform, both the binary and its checksum. Whatever
// verifies before replacing itself — the agent for its own family, the mother
// for its own — would find a build missing its .sha256 to be a rollout target
// that always fails.
//
// The families are collected separately, and a release may well carry one and
// not the other: every release published before the mother became publishable
// has agent builds only.
func buildsFrom(releases []apiRelease, includePrereleases bool) (agents, mother []Build) {
	agents = make([]Build, 0, len(releases))
	mother = make([]Build, 0, len(releases))
	for _, r := range releases {
		if r.Draft || (r.Prerelease && !includePrereleases) {
			continue
		}
		present := make(map[string]bool, len(r.Assets))
		for _, a := range r.Assets {
			present[a.Name] = true
		}
		var agentPlats, motherPlats []string
		for name := range present {
			kind, plat, ok := sharedrelease.AssetKindOf(name)
			if !ok || !present[name+sharedrelease.ChecksumSuffix] {
				continue
			}
			switch kind {
			case sharedrelease.KindAgent:
				agentPlats = append(agentPlats, plat)
			case sharedrelease.KindMother:
				motherPlats = append(motherPlats, plat)
			}
		}
		if len(agentPlats) > 0 {
			sort.Strings(agentPlats)
			agents = append(agents, Build{Version: r.TagName, Platforms: agentPlats})
		}
		if len(motherPlats) > 0 {
			sort.Strings(motherPlats)
			mother = append(mother, Build{Version: r.TagName, Platforms: motherPlats})
		}
	}
	SortDescending(agents)
	SortDescending(mother)
	return agents, mother
}

// SortDescending puts the newest version first, which is the order a rollout
// dropdown reads in. Exported because the local build catalogue orders its own
// list the same way, and a second comparator is how two lists begin disagreeing
// about which build is newest.
func SortDescending(builds []Build) {
	sort.Slice(builds, func(i, j int) bool {
		return naturalLess(builds[j].Version, builds[i].Version)
	})
}
