// Package release keeps the mother's view of what agent builds exist.
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

// Build is one downloadable agent version and the platforms it was built for.
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

// Fetch returns the current builds, or notModified when the list is unchanged
// since the last call.
//
// Unauthenticated GitHub allows 60 requests an hour per IP and does not count
// a conditional request answered 304. Polling without the ETag would spend a
// large part of that budget on a list that changes a few times a month — and
// run out during exactly the incident where someone is trying to roll back.
func (c *Client) Fetch(ctx context.Context) (builds []Build, notModified bool, err error) {
	url := c.apiBaseURL + "/repos/" + sharedrelease.Repo + "/releases?per_page=" + strconv.Itoa(perPage)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
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
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("github releases: %s", resp.Status)
	}

	var releases []apiRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodySize)).Decode(&releases); err != nil {
		return nil, false, fmt.Errorf("decode github releases: %w", err)
	}
	c.etag = resp.Header.Get("ETag")
	return buildsFrom(releases, c.includePrereleases), false, nil
}

// buildsFrom keeps only what an agent could actually install: a published
// release with, for at least one platform, both the binary and its checksum.
// The agent verifies before replacing itself, so a build missing its .sha256
// would be a rollout target that always fails.
func buildsFrom(releases []apiRelease, includePrereleases bool) []Build {
	out := make([]Build, 0, len(releases))
	for _, r := range releases {
		if r.Draft || (r.Prerelease && !includePrereleases) {
			continue
		}
		present := make(map[string]bool, len(r.Assets))
		for _, a := range r.Assets {
			present[a.Name] = true
		}
		var platforms []string
		for name := range present {
			plat, ok := sharedrelease.PlatformOf(name)
			if !ok || !present[name+sharedrelease.ChecksumSuffix] {
				continue
			}
			platforms = append(platforms, plat)
		}
		if len(platforms) == 0 {
			continue
		}
		sort.Strings(platforms)
		out = append(out, Build{Version: r.TagName, Platforms: platforms})
	}
	sort.Slice(out, func(i, j int) bool {
		return naturalLess(out[j].Version, out[i].Version) // descending
	})
	return out
}
