# Mother Self-Update Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator retarget the mother's own version from the admin panel, the way agents are already retargeted, and have the mother converge on that version by itself.

**Architecture:** The mother is an unprivileged, sandboxed systemd service, so it cannot replace its own binary. It downloads and SHA-256-verifies the new build from GitHub Releases, stages it inside the state directory it is allowed to write, and asks itself to shut down; a root `ExecStartPre=+` helper promotes the staged file into `/usr/local/bin/feast-watch` on the next start. The intent lives in a single-row `mother_update` table, and a bounded attempt counter makes a target that never lands stop instead of looping.

**Tech Stack:** Go 1.26 (stdlib + modernc sqlite), systemd, bash, GitHub Actions, Go/chi (backend proxy), React + Vite + vitest + @tanstack/react-query (panel).

**Spec:** `docs/superpowers/specs/2026-08-20-mother-self-update-design.md`

## Global Constraints

- **Repos.** `feast-watch` at `/Users/ceydaakin/GitHub/feast-watch`; backend proxy at `/Users/ceydaakin/feast/feast-mobile-backend`; panel at `/Users/ceydaakin/GitHub/feast-mobile-backend-control`.
- **The owner has asked that nothing be committed without their say-so.** Run the `git commit` step of each task only after they confirm; until then, finish the task at its passing-test step and move on.
- **Mother platforms are linux/amd64 and linux/arm64 only.** Agents keep all four (`linux-amd64`, `linux-arm64`, `windows-amd64`, `darwin-arm64`).
- **Asset names carry no version** — GitHub scopes assets to their release, and the tag in the URL identifies the build. Mother assets are `feast-watch-mother-<goos>-<goarch>`, agent assets `feast-watch-agent-<goos>-<goarch>`, each with a `.sha256` companion.
- **`"latest"` is never a valid rollout target** — it is a moving alias, so a process can never *be* it.
- **Nothing is installed without a verified checksum.** The checksum is fetched before the binary so a missing build costs one request, not a whole transfer.
- **Every user-facing string in the panel is Turkish.** Go error strings and comments are English.
- **`mother/store/settings.go:GetSettings` `Atoi`s every stored settings value** — no non-numeric value may ever be written to the `settings` table.
- **Tests:** Go `go test -race ./...` from the repo root; panel `npx vitest run`; shell `shellcheck` at its default (strict) severity, which is currently green and must stay green.
- **Update state values** are exactly `unsupported`, `idle`, `pending`, `failed`.
- **Attempt bound** is 3, counted across restarts, incremented and committed *before* the download.

---

## File Structure

**`feast-watch`**

| File | Responsibility |
|---|---|
| `shared/release/release.go` (modify) | Both asset families: names, platform lists, `AssetKindOf`, `ExpectedAssets` |
| `shared/selfupdate/selfupdate.go` (create) | Download + verify + atomically place a binary. No process control. |
| `shared/selfupdate/selfupdate_test.go` (create) | Its tests |
| `agent/update.go` (modify) | Keeps `SelfUpdate`/`exit(0)`; delegates the transfer to `shared/selfupdate` |
| `mother/release/github.go` (modify) | Split a release's assets into agent builds and mother builds |
| `mother/release/index.go` (modify) | `Snapshot` carries both lists |
| `mother/store/schema.go`, `migrate.go` (modify) | `mother_update` table, fresh and migrated |
| `mother/store/motherupdate.go` (create) | Reads and writes of that one row |
| `mother/selfupdate/updater.go` (create) | The convergence loop, boot reconcile, attempt bound, promote detection |
| `mother/api/mother.go` (create) | `PUT /api/mother/version` and its validation |
| `mother/api/versions.go` (modify) | `GET /api/version` extended |
| `mother/cmd/feast-watch/main.go` (modify) | Wire the updater, hand it a shutdown func |
| `deploy/feast-watch-mother-promote` (create) | Root promote helper |
| `deploy/feast-watch-mother.service` (modify) | `ExecStartPre=+…` |
| `deploy/mother-install.sh`, `mother-uninstall.sh` (modify) | Install/remove the helper, the staging dir and the `.bak`; manifest |
| `e2e/promote_test.sh` (create) | Promote helper against an `FW_ROOT` temp tree |
| `.github/workflows/release.yml`, `bin/release.sh` (modify) | Build and upload mother assets |
| `QUICKSTART.md` (modify) | How a mother is upgraded now |

**`feast-mobile-backend`**: `app/api/admin/monitoring_routes.go` (one row), `app/api/admin/monitoring.go` (one handler), plus their tests.

**`feast-mobile-backend-control`**: `src/api/monitoring.js` (`setMotherVersion`), `src/features/monitoring/MotherCard.jsx` + `UpdateMotherDialog.jsx` (create), `MonitoringPage.jsx` (mount the card), plus tests.

---

### Task 1: Two asset families in `shared/release`

**Files:**
- Modify: `shared/release/release.go`
- Modify: `shared/release/release_test.go`
- Modify: `mother/release/github.go:99` (the `PlatformOf` call site)

**Interfaces:**
- Consumes: nothing.
- Produces: `release.Kind` (`KindAgent`, `KindMother`), `release.MotherPlatforms []Platform`, `release.MotherAssetName(goos, goarch string) string`, `release.AssetKindOf(asset string) (Kind, string, bool)`, and `release.ExpectedAssets() []string` now covering both families. `release.PlatformOf` is **deleted**.

- [ ] **Step 1: Write the failing tests**

Append to `shared/release/release_test.go`, and delete the existing `TestPlatformOfRoundTripsEveryBuiltPlatform` and `TestPlatformOfRejectsCompanionsAndStrangers` (their replacements are below):

```go
func TestMotherAssetNameIsPlatformKeyed(t *testing.T) {
	if got := MotherAssetName("linux", "amd64"); got != "feast-watch-mother-linux-amd64" {
		t.Fatalf("mother asset name: %q", got)
	}
}

// The two families must never be confused for one another: an agent handed a
// mother build would replace itself with the control plane.
func TestAssetKindOfSeparatesTheFamilies(t *testing.T) {
	for _, tc := range []struct {
		asset string
		kind  Kind
		plat  string
	}{
		{"feast-watch-agent-linux-amd64", KindAgent, "linux-amd64"},
		{"feast-watch-agent-darwin-arm64", KindAgent, "darwin-arm64"},
		{"feast-watch-mother-linux-amd64", KindMother, "linux-amd64"},
		{"feast-watch-mother-linux-arm64", KindMother, "linux-arm64"},
	} {
		kind, plat, ok := AssetKindOf(tc.asset)
		if !ok || kind != tc.kind || plat != tc.plat {
			t.Fatalf("%q -> (%q, %q, %v)", tc.asset, kind, plat, ok)
		}
	}
}

func TestAssetKindOfRejectsCompanionsAndStrangers(t *testing.T) {
	for _, name := range []string{
		"feast-watch-agent-linux-amd64.sha256",  // the checksum companion
		"feast-watch-mother-linux-amd64.sha256", // ditto
		"feast-watch-agent",                     // no platform
		"feast-watch-mother",                    // no platform
		"feast-watch-mother-darwin-arm64",       // not a platform the mother is built for
		"feast-watch-agent-plan9-mips",
		"README.md",
		"",
	} {
		if kind, plat, ok := AssetKindOf(name); ok {
			t.Fatalf("%q must not parse as a build, got (%q, %q)", name, kind, plat)
		}
	}
}

func TestExpectedAssetsCoversBothFamilies(t *testing.T) {
	have := make(map[string]bool)
	for _, name := range ExpectedAssets() {
		if have[name] {
			t.Fatalf("duplicate asset name %q", name)
		}
		have[name] = true
	}
	if len(have) != 2*(len(Platforms)+len(MotherPlatforms)) {
		t.Fatalf("expected a binary and a checksum for every build, got %d", len(have))
	}
	for _, p := range Platforms {
		asset := AssetName(p.GOOS, p.GOARCH)
		if !have[asset] || !have[asset+ChecksumSuffix] {
			t.Fatalf("agent build %s not fully covered", p)
		}
	}
	for _, p := range MotherPlatforms {
		asset := MotherAssetName(p.GOOS, p.GOARCH)
		if !have[asset] || !have[asset+ChecksumSuffix] {
			t.Fatalf("mother build %s not fully covered", p)
		}
	}
}

// The mother runs as a systemd service on a Linux host: deploy/, the unit and
// the promote helper all assume it. Offering a darwin mother build would be a
// rollout target no supported deployment could apply.
func TestMotherIsBuiltForLinuxOnly(t *testing.T) {
	for _, p := range MotherPlatforms {
		if p.GOOS != "linux" {
			t.Fatalf("mother platform %s is not linux", p)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./shared/release/`
Expected: FAIL — `undefined: MotherAssetName`, `undefined: AssetKindOf`, `undefined: Kind`, `undefined: MotherPlatforms`.

- [ ] **Step 3: Implement**

In `shared/release/release.go`, add beside `assetPrefix`:

```go
// motherAssetPrefix is the stem every mother asset shares. A separate prefix,
// not a suffix or a directory, so one string comparison tells the two families
// apart at every point the name is read.
const motherAssetPrefix = "feast-watch-mother-"

// Kind is which binary an asset holds. The mother indexes both families from
// the same release and must never offer one where the other is meant.
type Kind string

const (
	KindAgent  Kind = "agent"
	KindMother Kind = "mother"
)

// MotherPlatforms is the mother's build matrix, deliberately narrower than
// Platforms. The mother is a systemd service on a Linux host — the unit file,
// deploy/ and the promote helper all assume it — so a darwin or windows build
// would be a rollout target nothing could apply.
var MotherPlatforms = []Platform{
	{"linux", "amd64"},
	{"linux", "arm64"},
}

// MotherAssetName is the release asset holding the mother for one platform.
func MotherAssetName(goos, goarch string) string {
	return motherAssetPrefix + goos + "-" + goarch
}
```

Replace `PlatformOf` entirely with:

```go
// AssetKindOf reads the family and platform back out of an asset name,
// reporting false for checksum companions, release notes, and any platform
// that family is not built for.
func AssetKindOf(asset string) (Kind, string, bool) {
	if strings.HasSuffix(asset, ChecksumSuffix) {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(asset, motherAssetPrefix); found {
		return KindMother, rest, contains(MotherPlatforms, rest)
	}
	if rest, found := strings.CutPrefix(asset, assetPrefix); found {
		return KindAgent, rest, contains(Platforms, rest)
	}
	return "", "", false
}

func contains(platforms []Platform, plat string) bool {
	for _, p := range platforms {
		if p.String() == plat {
			return true
		}
	}
	return false
}
```

Note `AssetKindOf` returns `("mother", "darwin-arm64", false)` for an unbuilt platform; every caller checks `ok` first, and the test above asserts the `ok=false`.

Extend `ExpectedAssets`:

```go
func ExpectedAssets() []string {
	out := make([]string, 0, 2*(len(Platforms)+len(MotherPlatforms)))
	for _, p := range Platforms {
		asset := AssetName(p.GOOS, p.GOARCH)
		out = append(out, asset, asset+ChecksumSuffix)
	}
	for _, p := range MotherPlatforms {
		asset := MotherAssetName(p.GOOS, p.GOARCH)
		out = append(out, asset, asset+ChecksumSuffix)
	}
	return out
}
```

In `mother/release/github.go`, inside `buildsFrom`, replace `plat, ok := sharedrelease.PlatformOf(name)` with `_, plat, ok := sharedrelease.AssetKindOf(name)` for now — Task 3 rewrites this function properly. This keeps the tree compiling between tasks.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./shared/release/ ./mother/release/ ./agent/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shared/release/ mother/release/github.go
git commit -m "feat(release): name and validate the mother's build family"
```

---

### Task 2: Publish the mother binary

**Files:**
- Modify: `.github/workflows/release.yml`
- Modify: `bin/release.sh`

**Interfaces:**
- Consumes: `release.MotherPlatforms`, `release.ExpectedAssets` (Task 1) — the `assert` job already reads the latter through `.github/tools/assetnames`.
- Produces: release assets `feast-watch-mother-linux-{amd64,arm64}` plus `.sha256`.

- [ ] **Step 1: Add the mother build job**

In `.github/workflows/release.yml`, after the `build` job and before `assert`, add:

```yaml
  # The mother's matrix is narrower than the agent's on purpose: it is a
  # systemd service on a Linux host, so a darwin build would be a rollout
  # target nothing could apply. Must match shared/release.MotherPlatforms —
  # the assert job reads that list back out of the Go package.
  build-mother:
    needs: prepare
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        include:
          - { goos: linux, goarch: amd64 }
          - { goos: linux, goarch: arm64 }
    steps:
      - uses: actions/checkout@v4
        with:
          ref: ${{ inputs.tag || github.ref }}
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: build mother
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
        run: |
          asset="feast-watch-mother-${GOOS}-${GOARCH}"
          CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o "$asset" ./mother/cmd/feast-watch
          # The mother refuses to stage a build it cannot verify, so an asset
          # uploaded without its checksum is a target that can only ever fail.
          sha256sum "$asset" | cut -d' ' -f1 > "$asset.sha256"

      - name: upload assets
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          asset="feast-watch-mother-${{ matrix.goos }}-${{ matrix.goarch }}"
          gh release upload "$TAG" "$asset" "$asset.sha256" --clobber
```

Change the `assert` job's `needs: build` to `needs: [build, build-mother]`.

- [ ] **Step 2: Build the mother's release binaries locally too**

In `bin/release.sh`, add beside `PLATFORMS`:

```bash
# Must match shared/release.MotherPlatforms and the release workflow's
# build-mother matrix.
MOTHER_PLATFORMS=(
  linux-amd64
  linux-arm64
)
```

After the agent build loop, and before the closing `echo`, add:

```bash
for platform in "${MOTHER_PLATFORMS[@]}"; do
  goos="${platform%-*}"
  goarch="${platform#*-}"

  # Named exactly as the release asset, so a locally built mother can be
  # uploaded to a release by hand if CI is unavailable.
  out="$OUT_DIR/feast-watch-mother-$platform"
  echo "-> building mother $platform"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -ldflags "$LDFLAGS" -o "$out" ./mother/cmd/feast-watch
  checksum "$out"
done
```

The existing `--mother-only` path is untouched: it builds one host-native binary for a host being deployed, which is a different job from publishing.

- [ ] **Step 3: Verify the script still works and lints**

Run:
```bash
shellcheck bin/release.sh
OUT_DIR=/tmp/relcheck bash bin/release.sh v0.0.0-test
ls /tmp/relcheck | sort > /tmp/built
go run ./.github/tools/assetnames | sort > /tmp/want
comm -13 /tmp/built /tmp/want   # must print nothing
```
Expected: shellcheck silent; `comm` prints nothing (every expected asset was built).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml bin/release.sh
git commit -m "feat(release): publish the mother binary alongside the agents"
```

---

### Task 3: Index mother builds separately

**Files:**
- Modify: `mother/release/github.go:96-125` (`buildsFrom`, `Build`)
- Modify: `mother/release/index.go` (`Snapshot`, `Cache`)
- Modify: `mother/release/github_test.go`, `mother/release/index_test.go`
- Modify: `mother/api/versions.go:47` (`handleGetVersion`, compile fix only — Task 7 extends it)

**Interfaces:**
- Consumes: `release.AssetKindOf`, `release.KindAgent`, `release.KindMother` (Task 1).
- Produces: `Snapshot{Builds []Build, Mother []Build, CheckedAt time.Time, Stale bool}`; `Source.Fetch(ctx) (agents, mother []Build, notModified bool, err error)`; `Cache.Snapshot() Snapshot` unchanged in name.

- [ ] **Step 1: Write the failing tests**

Append to `mother/release/github_test.go`:

```go
// One release carries both families. They are indexed apart because an agent
// handed a mother build would replace itself with the control plane.
func TestBuildsFromSplitsTheTwoFamilies(t *testing.T) {
	agents, mother := buildsFrom([]apiRelease{{
		TagName: "v1.4.0",
		Assets: assetNames(
			"feast-watch-agent-linux-amd64", "feast-watch-agent-linux-amd64.sha256",
			"feast-watch-agent-darwin-arm64", "feast-watch-agent-darwin-arm64.sha256",
			"feast-watch-mother-linux-amd64", "feast-watch-mother-linux-amd64.sha256",
		),
	}}, false)

	if len(agents) != 1 || len(agents[0].Platforms) != 2 {
		t.Fatalf("agents: %+v", agents)
	}
	if len(mother) != 1 || len(mother[0].Platforms) != 1 || mother[0].Platforms[0] != "linux-amd64" {
		t.Fatalf("mother: %+v", mother)
	}
}

// The same rule the agent family has always had: a binary with no checksum is
// a target that can only ever fail, so it is not offered at all.
func TestBuildsFromDropsAMotherBinaryWithNoChecksum(t *testing.T) {
	agents, mother := buildsFrom([]apiRelease{{
		TagName: "v1.4.0",
		Assets: assetNames(
			"feast-watch-agent-linux-amd64", "feast-watch-agent-linux-amd64.sha256",
			"feast-watch-mother-linux-amd64", // no .sha256
		),
	}}, false)

	if len(agents) != 1 {
		t.Fatalf("the agent build must survive: %+v", agents)
	}
	if len(mother) != 0 {
		t.Fatalf("a mother binary with no checksum must not be offered: %+v", mother)
	}
}

// A release with only agent builds is the entire published history up to
// v1.0.1, and it must index cleanly rather than producing an empty mother
// entry the panel would render as a selectable version.
func TestBuildsFromOmitsAReleaseWithNoMotherBuild(t *testing.T) {
	_, mother := buildsFrom([]apiRelease{{
		TagName: "v1.0.1",
		Assets: assetNames(
			"feast-watch-agent-linux-amd64", "feast-watch-agent-linux-amd64.sha256",
		),
	}}, false)
	if len(mother) != 0 {
		t.Fatalf("mother: %+v", mother)
	}
}

// assetNames builds the anonymous asset slice apiRelease declares.
func assetNames(names ...string) []struct {
	Name string `json:"name"`
} {
	out := make([]struct {
		Name string `json:"name"`
	}, 0, len(names))
	for _, n := range names {
		out = append(out, struct {
			Name string `json:"name"`
		}{Name: n})
	}
	return out
}
```

Append to `mother/release/index_test.go`:

```go
// Both lists are replaced together: a refresh that updated one and kept the
// other would let the panel offer a mother version from a release list that no
// longer describes what is published.
func TestSnapshotCarriesBothFamilies(t *testing.T) {
	c := NewCache(stubSource{
		agents: []Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64"}}},
		mother: []Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64", "linux-arm64"}}},
	}, func() time.Time { return time.Unix(1000, 0) })

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := c.Snapshot()
	if len(snap.Builds) != 1 || len(snap.Mother) != 1 {
		t.Fatalf("snapshot: %+v", snap)
	}
	if len(snap.Mother[0].Platforms) != 2 {
		t.Fatalf("mother platforms: %+v", snap.Mother[0])
	}
}

// Handing out the cache's own slices would let a handler's incidental write
// reach shared state — the guarantee Builds already had, now for Mother too.
func TestSnapshotCopiesTheMotherList(t *testing.T) {
	c := NewCache(stubSource{
		mother: []Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64"}}},
	}, time.Now)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := c.Snapshot()
	first.Mother[0].Platforms[0] = "tampered"
	if c.Snapshot().Mother[0].Platforms[0] != "linux-amd64" {
		t.Fatal("Snapshot handed out the cache's own slice")
	}
}

type stubSource struct {
	agents, mother []Build
	notModified    bool
	err            error
}

func (s stubSource) Fetch(context.Context) ([]Build, []Build, bool, error) {
	return s.agents, s.mother, s.notModified, s.err
}
```

If `index_test.go` already declares a stub source, rename that one's methods to the new signature instead of adding a second type.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./mother/release/`
Expected: FAIL — `buildsFrom` returns one value, `Snapshot` has no field `Mother`, `Fetch` signature mismatch.

- [ ] **Step 3: Implement**

`mother/release/github.go` — `Source`, `Fetch` and `buildsFrom` return both families:

```go
// Source is what the cache reads from; the GitHub Client implements it. The
// two families come back apart rather than tagged in one list: every consumer
// wants exactly one of them, and a Kind field would put the separation at each
// call site instead of here.
type Source interface {
	Fetch(context.Context) (agents, mother []Build, notModified bool, err error)
}
```

```go
func (c *Client) Fetch(ctx context.Context) (agents, mother []Build, notModified bool, err error) {
	// ... request construction unchanged ...
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
```

```go
// buildsFrom keeps only what could actually be installed: a published release
// with, for at least one platform, both the binary and its checksum. Each
// family is collected separately — a release may well carry agent builds and
// no mother build, which is every release published before this existed.
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
	sortDescending(agents)
	sortDescending(mother)
	return agents, mother
}

func sortDescending(builds []Build) {
	sort.Slice(builds, func(i, j int) bool {
		return naturalLess(builds[j].Version, builds[i].Version)
	})
}
```

`mother/release/index.go` — `Snapshot`, `Seed`, `Refresh` and `Snapshot()` carry both:

```go
type Snapshot struct {
	Builds []Build `json:"builds"`
	// Mother is the same list for the mother's own binary. Separate rather
	// than merged: the mother is built for fewer platforms and is targeted
	// through a different endpoint, and merging would make "which versions
	// exist" ambiguous at every read.
	Mother    []Build   `json:"mother"`
	CheckedAt time.Time `json:"checked_at"`
	Stale     bool      `json:"stale"`
}
```

```go
func NewCache(src Source, now func() time.Time) *Cache {
	return &Cache{
		src:  src,
		now:  now,
		snap: Snapshot{Builds: []Build{}, Mother: []Build{}, Stale: true},
	}
}

// Seed publishes an agent build list supplied by configuration, so a mother
// with no route to github.com can still roll agents out. It seeds no mother
// builds: a mother that cannot reach the release host cannot download its own
// replacement either, so offering itself a target would only produce a
// bounded run of failed attempts.
func (c *Cache) Seed(builds []Build) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = Snapshot{Builds: cloneBuilds(builds), Mother: []Build{}, CheckedAt: c.now(), Stale: false}
}

func (c *Cache) Refresh(ctx context.Context) error {
	agents, mother, notModified, err := c.src.Fetch(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.snap = Snapshot{Builds: c.snap.Builds, Mother: c.snap.Mother, CheckedAt: c.snap.CheckedAt, Stale: true}
		return err
	}
	if notModified {
		c.snap = Snapshot{Builds: c.snap.Builds, Mother: c.snap.Mother, CheckedAt: c.now(), Stale: false}
		return nil
	}
	c.snap = Snapshot{Builds: cloneBuilds(agents), Mother: cloneBuilds(mother), CheckedAt: c.now(), Stale: false}
	return nil
}

func (c *Cache) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Snapshot{
		Builds:    cloneBuilds(c.snap.Builds),
		Mother:    cloneBuilds(c.snap.Mother),
		CheckedAt: c.snap.CheckedAt,
		Stale:     c.snap.Stale,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test -race ./mother/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add mother/release/
git commit -m "feat(release): index the mother's builds apart from the agents'"
```

---

### Task 4: The `mother_update` row

**Files:**
- Create: `mother/store/motherupdate.go`
- Create: `mother/store/motherupdate_test.go`
- Modify: `mother/store/schema.go`
- Modify: `mother/store/migrate.go`
- Modify: `mother/store/migrate_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:

```go
type MotherUpdate struct {
	DesiredVersion string
	StagedVersion  string
	Attempts       int
	Error          string
	RequestedAt    int64
	AppliedAt      int64
}

func (s *Store) MotherUpdate() (MotherUpdate, error)
func (s *Store) SetMotherDesiredVersion(version string, now int64) error
func (s *Store) BeginMotherAttempt(now int64) (int, error)
func (s *Store) StageMotherUpdate(version string) error
func (s *Store) RecordMotherUpdateError(msg string) error
func (s *Store) FailMotherUpdate(msg string) error
func (s *Store) ClearMotherUpdate(appliedAt int64) error
```

- [ ] **Step 1: Write the failing tests**

Create `mother/store/motherupdate_test.go`:

```go
package store

import "testing"

func TestMotherUpdateStartsEmpty(t *testing.T) {
	s := testStore(t)
	got, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got != (MotherUpdate{}) {
		t.Fatalf("a fresh database must hold no update intent: %+v", got)
	}
}

// A fresh decision by an operator must not inherit the attempt budget a
// previous one spent, nor the error that explained why it stopped.
func TestSetMotherDesiredVersionResetsTheAttemptBudget(t *testing.T) {
	s := testStore(t)
	if err := s.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginMotherAttempt(101); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMotherUpdateError("checksum mismatch"); err != nil {
		t.Fatal(err)
	}
	if err := s.StageMotherUpdate("v1.4.0"); err != nil {
		t.Fatal(err)
	}

	if err := s.SetMotherDesiredVersion("v1.5.0", 200); err != nil {
		t.Fatal(err)
	}
	got, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredVersion != "v1.5.0" || got.Attempts != 0 || got.Error != "" || got.StagedVersion != "" {
		t.Fatalf("a new target must start clean: %+v", got)
	}
	if got.RequestedAt != 200 {
		t.Fatalf("requested_at: %d", got.RequestedAt)
	}
}

// The counter is what bounds a mother that restarts into the same failure, so
// it has to survive the restart — i.e. be committed, not held in memory.
func TestBeginMotherAttemptCountsUpAndPersists(t *testing.T) {
	s := testStore(t)
	if err := s.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 3; want++ {
		got, err := s.BeginMotherAttempt(int64(100 + want))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("attempt %d reported as %d", want, got)
		}
	}
	row, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.Attempts != 3 {
		t.Fatalf("attempts: %d", row.Attempts)
	}
}

// Failing drops the target but keeps the reason: the target is what would
// otherwise be retried forever, the reason is the only thing that explains it.
func TestFailMotherUpdateDropsTheTargetAndKeepsTheReason(t *testing.T) {
	s := testStore(t)
	if err := s.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.FailMotherUpdate("giving up after 3 attempts"); err != nil {
		t.Fatal(err)
	}
	got, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredVersion != "" {
		t.Fatalf("target must be cleared: %+v", got)
	}
	if got.Error != "giving up after 3 attempts" {
		t.Fatalf("error: %q", got.Error)
	}
}

func TestClearMotherUpdateResetsEverythingAndStampsApplied(t *testing.T) {
	s := testStore(t)
	if err := s.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginMotherAttempt(101); err != nil {
		t.Fatal(err)
	}
	if err := s.StageMotherUpdate("v1.4.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearMotherUpdate(300); err != nil {
		t.Fatal(err)
	}

	got, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredVersion != "" || got.StagedVersion != "" || got.Attempts != 0 || got.Error != "" {
		t.Fatalf("row must be idle: %+v", got)
	}
	if got.AppliedAt != 300 {
		t.Fatalf("applied_at: %d", got.AppliedAt)
	}
}
```

Use whatever helper the package already has for a temp store; if `testStore(t)` does not exist, copy the construction used at the top of `mother/store/settings_test.go` into a `testStore` helper in this file.

Append to `mother/store/migrate_test.go`:

```go
// A live database predates this table. Migration has to add it, and adding it
// twice must be a no-op — Migrate runs on every boot.
func TestMigrateAddsMotherUpdateToAnOlderDatabase(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE mother_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if _, err := s.MotherUpdate(); err != nil {
		t.Fatalf("mother_update unusable after migration: %v", err)
	}
}
```

Adjust the `PRAGMA user_version` reset and the `Migrate` call to whatever `migrate.go` actually exposes — read it first; the shape above assumes an exported `Migrate()` called from `Open`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./mother/store/`
Expected: FAIL — `undefined: MotherUpdate`, `no such table: mother_update`.

- [ ] **Step 3: Implement**

In `mother/store/schema.go`, add to the `schema` const:

```sql
-- The mother's own rollout intent. One row, enforced by the CHECK: there is
-- one mother per deployment and "which version should I be" is a property of
-- the process, not of a collection.
--
-- Deliberately NOT in `settings`: GetSettings runs strconv.Atoi over every
-- stored value, so a version string there would not merely display wrong — it
-- would fail every settings read in the mother, taking the heartbeat threshold
-- and the retention sweep down with it.
CREATE TABLE IF NOT EXISTS mother_update (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  desired_version TEXT    NOT NULL DEFAULT '',
  staged_version  TEXT    NOT NULL DEFAULT '',
  attempts        INTEGER NOT NULL DEFAULT 0,
  error           TEXT    NOT NULL DEFAULT '',
  requested_at    INTEGER NOT NULL DEFAULT 0,
  applied_at      INTEGER NOT NULL DEFAULT 0
);
```

In `mother/store/migrate.go`, add the same `CREATE TABLE IF NOT EXISTS` statement as a new migration step, following the file's existing `user_version` pattern exactly (read the neighbouring step for the shape — groups added one the same way).

Create `mother/store/motherupdate.go`:

```go
package store

// MotherUpdate is the mother's own rollout intent and how far it has got.
//
// The zero value is "nothing to do", which is what both a fresh database and a
// finished update read as — there is no separate idle marker to keep in step.
type MotherUpdate struct {
	DesiredVersion string
	StagedVersion  string
	Attempts       int
	Error          string
	RequestedAt    int64
	AppliedAt      int64
}

// motherUpdateRow is the singleton's id. Named rather than inlined so the
// CHECK constraint in the schema and every statement here agree by
// construction.
const motherUpdateRow = 1

func (s *Store) MotherUpdate() (MotherUpdate, error) {
	var out MotherUpdate
	err := s.db.QueryRow(
		`SELECT desired_version, staged_version, attempts, error, requested_at, applied_at
		   FROM mother_update WHERE id = ?`, motherUpdateRow).
		Scan(&out.DesiredVersion, &out.StagedVersion, &out.Attempts, &out.Error,
			&out.RequestedAt, &out.AppliedAt)
	if err == sql.ErrNoRows {
		// The row is created on first write, so its absence is the zero
		// intent rather than a fault.
		return MotherUpdate{}, nil
	}
	return out, err
}

// SetMotherDesiredVersion records a target, resetting everything a previous
// target left behind. An operator's fresh decision must not inherit the
// attempt budget the last one spent, or the error that explained why it
// stopped. An empty version cancels.
func (s *Store) SetMotherDesiredVersion(version string, now int64) error {
	_, err := s.db.Exec(
		`INSERT INTO mother_update (id, desired_version, staged_version, attempts, error, requested_at)
		 VALUES (?, ?, '', 0, '', ?)
		 ON CONFLICT(id) DO UPDATE SET
		   desired_version = excluded.desired_version,
		   staged_version  = '',
		   attempts        = 0,
		   error           = '',
		   requested_at    = excluded.requested_at`,
		motherUpdateRow, version, now)
	return err
}

// BeginMotherAttempt counts one attempt against the target and returns the new
// total. It is called — and committed — BEFORE the download, because a counter
// written after a step that never completes counts nothing, and this counter
// is the only thing standing between a bad target and a download-exit-restart
// loop that never explains itself.
func (s *Store) BeginMotherAttempt(now int64) (int, error) {
	if _, err := s.db.Exec(
		`UPDATE mother_update SET attempts = attempts + 1, error = '' WHERE id = ?`,
		motherUpdateRow); err != nil {
		return 0, err
	}
	row, err := s.MotherUpdate()
	return row.Attempts, err
}

// StageMotherUpdate records that a verified binary is waiting for the promote
// helper to install it on the next start.
func (s *Store) StageMotherUpdate(version string) error {
	_, err := s.db.Exec(
		`UPDATE mother_update SET staged_version = ? WHERE id = ?`, version, motherUpdateRow)
	return err
}

// RecordMotherUpdateError leaves the target in place: the failure may well be
// transient, and the attempt counter is what bounds the retrying.
func (s *Store) RecordMotherUpdateError(msg string) error {
	_, err := s.db.Exec(
		`UPDATE mother_update SET error = ? WHERE id = ?`, msg, motherUpdateRow)
	return err
}

// FailMotherUpdate gives up on the target but keeps the reason. Dropping both
// would leave an operator with a mother that quietly stayed where it was.
func (s *Store) FailMotherUpdate(msg string) error {
	_, err := s.db.Exec(
		`UPDATE mother_update SET desired_version = '', staged_version = '', error = ? WHERE id = ?`,
		msg, motherUpdateRow)
	return err
}

// ClearMotherUpdate is the successful end: the running version is the wanted
// one, so nothing about the last update is still true except when it landed.
func (s *Store) ClearMotherUpdate(appliedAt int64) error {
	_, err := s.db.Exec(
		`INSERT INTO mother_update (id, applied_at) VALUES (?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   desired_version = '', staged_version = '', attempts = 0, error = '',
		   applied_at = excluded.applied_at`,
		motherUpdateRow, appliedAt)
	return err
}
```

Add `"database/sql"` to the imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./mother/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add mother/store/
git commit -m "feat(store): a single-row table for the mother's own rollout intent"
```

---

### Task 5: Share the download with the agent

**Files:**
- Create: `shared/selfupdate/selfupdate.go`
- Create: `shared/selfupdate/selfupdate_test.go`
- Modify: `agent/update.go`
- Verify unchanged: `agent/update_test.go`

**Interfaces:**
- Consumes: `release.DownloadURL`, `release.ParseChecksum`, `release.ChecksumSuffix`.
- Produces: `selfupdate.Place(client *http.Client, baseURL, tag, asset, dest string) error`.

- [ ] **Step 1: Write the failing test**

Create `shared/selfupdate/selfupdate_test.go`:

```go
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseHost serves one asset and its checksum the way GitHub Releases does.
func releaseHost(t *testing.T, body []byte, sum string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			w.Write([]byte(sum + "  asset\n"))
		case strings.HasSuffix(r.URL.Path, "/asset"):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPlaceWritesAnExecutableWhenTheChecksumMatches(t *testing.T) {
	body := []byte("#!/bin/true\n")
	srv := releaseHost(t, body, digest(body))
	dest := filepath.Join(t.TempDir(), "feast-watch")

	if err := Place(srv.Client(), srv.URL, "v1.4.0", "asset", dest); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("content: %q", got)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode: %v — a replacement that is not executable cannot be started", info.Mode())
	}
}

// The whole point of the checksum: a corrupted or substituted binary must not
// reach the destination at all, and must not be left lying beside it either.
func TestPlaceRefusesAMismatchAndLeavesNothingBehind(t *testing.T) {
	srv := releaseHost(t, []byte("tampered"), digest([]byte("expected")))
	dir := t.TempDir()
	dest := filepath.Join(dir, "feast-watch")

	err := Place(srv.Client(), srv.URL, "v1.4.0", "asset", dest)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err: %v", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("a binary that failed verification was placed anyway")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// The checksum is fetched first so a tag that was never published, or one with
// no build for this platform, fails for the price of one request.
func TestPlaceFailsOnAMissingChecksumBeforeTransferringAnything(t *testing.T) {
	var assetHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".sha256") {
			assetHits++
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "feast-watch")
	if err := Place(srv.Client(), srv.URL, "v9.9.9", "asset", dest); err == nil {
		t.Fatal("expected a failure for an unpublished tag")
	}
	if assetHits != 0 {
		t.Fatalf("the binary was requested %d times despite no checksum", assetHits)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./shared/selfupdate/`
Expected: FAIL — no such package / `undefined: Place`.

- [ ] **Step 3: Implement by moving, not rewriting**

Create `shared/selfupdate/selfupdate.go` holding the code currently in `agent/update.go`, unchanged apart from names and the removal of process control:

```go
// Package selfupdate downloads a released binary, verifies its published
// SHA-256, and puts it somewhere.
//
// It does not restart anything, and it does not care what the binary is: the
// agent points it at its own executable and then exits; the mother points it
// at a staging path inside its state directory, because a sandboxed service
// cannot write its own binary and a root helper installs it on the next start.
// Sharing the transfer is what makes "the mother updates the way the agent
// does" a fact about the code rather than a claim in a comment.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/osman-yahya/feast-watch/shared/release"
)

const (
	maxBinarySize   = 256 << 20 // 256 MB cap on downloaded binaries
	maxChecksumSize = 1 << 10   // 1 KB cap on .sha256 files
)

// Place fetches asset from the tagged release, verifies it against the
// published checksum, and renames it over dest.
//
// The checksum comes first, and it is small. A tag that was never published,
// or one with no build for this platform, fails here for the price of one
// request instead of after a whole binary transfer.
func Place(client *http.Client, baseURL, tag, asset, dest string) error {
	sumRaw, err := fetch(client, release.DownloadURL(baseURL, tag, asset+release.ChecksumSuffix), maxChecksumSize)
	if err != nil {
		return err
	}
	wantSum, err := release.ParseChecksum(sumRaw)
	if err != nil {
		return fmt.Errorf("checksum for %s %s: %w", tag, asset, err)
	}

	tmp, gotSum, err := download(client, release.DownloadURL(baseURL, tag, asset), dest)
	if err != nil {
		return err
	}
	// From here every failure has to remove the temporary file: nothing else
	// ever sweeps it, so a stranded one accumulates on each retry.
	defer os.Remove(tmp)

	if gotSum != wantSum {
		return fmt.Errorf("checksum mismatch for %s %s: refusing update", tag, asset)
	}
	return os.Rename(tmp, dest)
}
```

Then move `fetch` and `download` across verbatim from `agent/update.go`, including their comments; rename `download`'s `target` parameter to `dest`.

Reduce `agent/update.go` to the process-control half:

```go
// selfUpdate fetches the build for this platform from the release host.
//
// The binary comes from GitHub Releases, never from the mother. The mother
// names a version and nothing more, which keeps binary distribution off the
// monitoring path entirely: the mother stores no builds, serves no bytes, and
// a rollout cannot be blocked by a file somebody forgot to stage on it.
//
// The transfer itself lives in shared/selfupdate, which the mother uses too.
// The agent is root and unsandboxed, so its destination is its own executable
// and the restart is systemd's answer to exiting 0.
func selfUpdate(cfg Config, desiredVersion, target string, exit func(int), client *http.Client) error {
	asset := release.AssetName(runtime.GOOS, runtime.GOARCH)
	if err := selfupdate.Place(client, cfg.releaseBaseURL(), desiredVersion, asset, target); err != nil {
		return err
	}
	exit(0)
	return nil
}
```

Delete `fetch`, `download`, `maxBinarySize` and `maxChecksumSize` from `agent/update.go`; drop the now-unused imports (`crypto/sha256`, `encoding/hex`, `io`, `os`, `path/filepath`).

- [ ] **Step 4: Run the tests, including the agent's untouched suite**

Run: `go test -race ./shared/selfupdate/ ./agent/`
Expected: PASS, with `agent/update_test.go` unmodified. If an agent test fails on an error string, the move changed a message — restore the original wording rather than editing the test.

- [ ] **Step 5: Commit**

```bash
git add shared/selfupdate/ agent/update.go
git commit -m "refactor(selfupdate): share the verified download between agent and mother"
```

---

### Task 6: The convergence loop

**Files:**
- Create: `mother/selfupdate/updater.go`
- Create: `mother/selfupdate/updater_test.go`

**Interfaces:**
- Consumes: `store.MotherUpdate` and the six writers (Task 4), `selfupdate.Place` (Task 5), `release.MotherAssetName` (Task 1).
- Produces:

```go
type Config struct {
	ReleaseBaseURL string
	PromotePath    string
	StageDir       string
	Platform       string        // "linux-amd64"
	MaxAttempts    int
	Interval       time.Duration
}

func New(st *store.Store, cfg Config, client *http.Client, now func() time.Time, shutdown func()) *Updater
func (u *Updater) Supported() bool
func (u *Updater) Platform() string
func (u *Updater) Reconcile(running string) error
func (u *Updater) Tick(running string) error
func (u *Updater) Run(ctx context.Context, running string)
```

`StagedPath()` is `filepath.Join(cfg.StageDir, "feast-watch.new")`.

- [ ] **Step 1: Write the failing tests**

Create `mother/selfupdate/updater_test.go`:

```go
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
)

const runningVersion = "v1.3.0"

// newFixture returns an updater whose release host serves body for every
// mother asset, with a promote helper present unless promote is "".
func newFixture(t *testing.T, body []byte, sum string, promote string) (*Updater, *store.Store, *int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			w.Write([]byte(sum + "\n"))
		case strings.Contains(r.URL.Path, "feast-watch-mother-"):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	st := testStore(t)
	shutdowns := 0
	u := New(st, Config{
		ReleaseBaseURL: srv.URL,
		PromotePath:    promote,
		StageDir:       dir,
		Platform:       "linux-amd64",
		MaxAttempts:    3,
		Interval:       time.Millisecond,
	}, srv.Client(), func() time.Time { return time.Unix(1000, 0) }, func() { shutdowns++ })
	return u, st, &shutdowns
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func promoteHelper(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "promote")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTickStagesTheBinaryAndAsksToShutDown(t *testing.T) {
	body := []byte("new mother")
	u, st, shutdowns := newFixture(t, body, digest(body), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}

	if err := u.Tick(runningVersion); err != nil {
		t.Fatal(err)
	}

	staged, err := os.ReadFile(u.StagedPath())
	if err != nil {
		t.Fatalf("nothing staged: %v", err)
	}
	if string(staged) != string(body) {
		t.Fatalf("staged content: %q", staged)
	}
	if *shutdowns != 1 {
		t.Fatalf("shutdown requested %d times", *shutdowns)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.StagedVersion != "v1.4.0" || row.Attempts != 1 {
		t.Fatalf("row: %+v", row)
	}
}

// Nothing to do is the common case, and it must cost one row read and no
// network at all.
func TestTickDoesNothingWithoutATarget(t *testing.T) {
	u, _, shutdowns := newFixture(t, []byte("x"), digest([]byte("x")), promoteHelper(t))
	if err := u.Tick(runningVersion); err != nil {
		t.Fatal(err)
	}
	if *shutdowns != 0 {
		t.Fatal("shut down with no target set")
	}
	if _, err := os.Stat(u.StagedPath()); err == nil {
		t.Fatal("staged a binary with no target set")
	}
}

func TestTickIsANoOpWhenTheTargetIsAlreadyRunning(t *testing.T) {
	u, st, shutdowns := newFixture(t, []byte("x"), digest([]byte("x")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion(runningVersion, 900); err != nil {
		t.Fatal(err)
	}
	if err := u.Tick(runningVersion); err != nil {
		t.Fatal(err)
	}
	if *shutdowns != 0 {
		t.Fatal("restarted to install the version already running")
	}
}

// A corrupt or substituted binary must never be staged, and the target stays:
// the failure may be transient, and the attempt counter bounds the retrying.
func TestTickRefusesAChecksumMismatchAndKeepsTheTarget(t *testing.T) {
	u, st, shutdowns := newFixture(t, []byte("tampered"), digest([]byte("expected")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}

	if err := u.Tick(runningVersion); err == nil {
		t.Fatal("expected a checksum failure")
	}
	if *shutdowns != 0 {
		t.Fatal("shut down despite staging nothing")
	}
	if _, err := os.Stat(u.StagedPath()); err == nil {
		t.Fatal("a binary that failed verification was staged")
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "v1.4.0" {
		t.Fatalf("target must survive a transient failure: %+v", row)
	}
	if !strings.Contains(row.Error, "checksum mismatch") {
		t.Fatalf("error: %q", row.Error)
	}
}

// The bound is what stops a mother that restarts into the same failure from
// downloading and exiting forever.
func TestTickGivesUpAfterMaxAttempts(t *testing.T) {
	u, st, _ := newFixture(t, []byte("tampered"), digest([]byte("expected")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := u.Tick(runningVersion); err == nil {
			t.Fatalf("attempt %d unexpectedly succeeded", i+1)
		}
	}
	// The fourth tick must not start a fourth attempt.
	if err := u.Tick(runningVersion); err != nil {
		t.Fatalf("the giving-up tick must not itself be an error: %v", err)
	}

	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "" {
		t.Fatalf("target must be dropped after the bound: %+v", row)
	}
	if row.Attempts != 3 {
		t.Fatalf("attempts: %d", row.Attempts)
	}
	if row.Error == "" {
		t.Fatal("giving up must leave a reason")
	}
}

// Docker has no systemd and no promote hook: a staged binary is discarded and
// the old version comes back. Offering the update there would be a button
// whose only effect is a restart.
func TestUnsupportedWithoutThePromoteHelper(t *testing.T) {
	u, st, shutdowns := newFixture(t, []byte("x"), digest([]byte("x")), "/nonexistent/promote")
	if u.Supported() {
		t.Fatal("Supported() must be false with no promote helper")
	}
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}
	if err := u.Tick(runningVersion); err != nil {
		t.Fatal(err)
	}
	if *shutdowns != 0 {
		t.Fatal("staged an update a deployment cannot promote")
	}
}

// The boot half: the new binary is the proof the update landed.
func TestReconcileClearsTheRowWhenTheTargetIsRunning(t *testing.T) {
	u, st, _ := newFixture(t, []byte("x"), digest([]byte("x")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginMotherAttempt(901); err != nil {
		t.Fatal(err)
	}

	if err := u.Reconcile("v1.4.0"); err != nil {
		t.Fatal(err)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "" || row.Attempts != 0 || row.Error != "" {
		t.Fatalf("row must be idle after a landed update: %+v", row)
	}
	if row.AppliedAt != 1000 {
		t.Fatalf("applied_at: %d", row.AppliedAt)
	}
}

// A staged binary that did not become the running version means the promote
// step did not happen — a missing helper, or one that could not write.
func TestReconcileReportsAStagedUpdateThatDidNotTake(t *testing.T) {
	u, st, _ := newFixture(t, []byte("x"), digest([]byte("x")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}
	if err := st.StageMotherUpdate("v1.4.0"); err != nil {
		t.Fatal(err)
	}

	if err := u.Reconcile(runningVersion); err != nil {
		t.Fatal(err)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.Error == "" {
		t.Fatal("a staged update that did not take must say so")
	}
	if row.DesiredVersion != "v1.4.0" {
		t.Fatalf("the target survives until the attempt bound: %+v", row)
	}
}
```

Add a `testStore(t *testing.T) *store.Store` helper to this file that opens a store on `filepath.Join(t.TempDir(), "mother.db")` — mirror what `mother/store`'s own tests do.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./mother/selfupdate/`
Expected: FAIL — no such package.

- [ ] **Step 3: Implement**

Create `mother/selfupdate/updater.go`:

```go
// Package selfupdate converges the mother on the version an operator picked in
// the panel.
//
// The mother cannot replace its own binary: its unit runs it as an
// unprivileged user under ProtectSystem=strict, so /usr/local/bin is read-only
// inside its namespace whatever the file permissions say. So this half does
// what it is allowed to do — download, verify, stage inside StateDirectory —
// and then asks the process to shut down. A root ExecStartPre helper installs
// the staged binary on the next start, before ExecStart runs it.
package selfupdate

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
	sharedrelease "github.com/osman-yahya/feast-watch/shared/release"
	"github.com/osman-yahya/feast-watch/shared/selfupdate"
)

// stagedName is what the promote helper looks for. Shared with
// deploy/feast-watch-mother-promote by convention, and asserted by
// e2e/promote_test.sh.
const stagedName = "feast-watch.new"

type Config struct {
	ReleaseBaseURL string
	// PromotePath is the root helper that installs a staged binary at the next
	// start. Its absence is how a deployment says it cannot self-update.
	PromotePath string
	StageDir    string
	// Platform is this mother's "<goos>-<goarch>", used both to pick the asset
	// and to reject a target with no build for it.
	Platform    string
	MaxAttempts int
	Interval    time.Duration
}

type Updater struct {
	st       *store.Store
	cfg      Config
	client   *http.Client
	now      func() time.Time
	shutdown func()
}

func New(st *store.Store, cfg Config, client *http.Client, now func() time.Time, shutdown func()) *Updater {
	return &Updater{st: st, cfg: cfg, client: client, now: now, shutdown: shutdown}
}

// StagedPath is where a verified binary waits for the promote helper.
func (u *Updater) StagedPath() string { return filepath.Join(u.cfg.StageDir, stagedName) }

// Platform is this mother's build platform, so the API can reject a target
// that was never built for it and name what it wanted.
func (u *Updater) Platform() string { return u.cfg.Platform }

// Supported reports whether this deployment can actually apply an update.
//
// In a container there is no systemd and no promote hook: the process would
// stage a binary, exit, and come back from the image on the old version. That
// is a worse answer than refusing, so the refusal is structural — the image
// does not ship the helper.
func (u *Updater) Supported() bool {
	if u.cfg.PromotePath == "" {
		return false
	}
	info, err := os.Stat(u.cfg.PromotePath)
	return err == nil && !info.IsDir()
}

// Reconcile settles what the last boot's update did. It runs once at startup,
// before the loop, because the mother is offline while it updates and this is
// the only moment it can report on itself.
func (u *Updater) Reconcile(running string) error {
	row, err := u.st.MotherUpdate()
	if err != nil {
		return err
	}
	if row.DesiredVersion == "" {
		return nil
	}
	if row.DesiredVersion == running {
		slog.Info("mother self-update landed", "version", running)
		return u.st.ClearMotherUpdate(u.now().Unix())
	}
	if row.StagedVersion != "" {
		// Staged, restarted, still not it. The promote step did not happen —
		// a missing helper, or one that could not write. Saying so is the
		// difference between a diagnosable failure and a silent loop.
		if err := u.st.RecordMotherUpdateError(fmt.Sprintf(
			"staged %s but %s is still running: the promote step did not take",
			row.StagedVersion, running)); err != nil {
			return err
		}
	}
	return nil
}

// Tick is one convergence step: at most one download, and at most one request
// to shut down.
func (u *Updater) Tick(running string) error {
	row, err := u.st.MotherUpdate()
	if err != nil {
		return err
	}
	if row.DesiredVersion == "" || row.DesiredVersion == running {
		return nil
	}
	if !u.Supported() {
		// The API refuses to set a target here, so reaching this means the
		// helper was removed after one was set. Do nothing rather than loop.
		return nil
	}
	if row.Attempts >= u.cfg.MaxAttempts {
		return u.st.FailMotherUpdate(fmt.Sprintf(
			"gave up on %s after %d attempts: %s",
			row.DesiredVersion, row.Attempts, lastReason(row.Error)))
	}

	// Committed BEFORE the download. A counter written after a step that never
	// completes counts nothing, and this counter is the only thing between a
	// bad target and download-exit-restart forever.
	attempt, err := u.st.BeginMotherAttempt(u.now().Unix())
	if err != nil {
		return err
	}

	goos, goarch, _ := strings.Cut(u.cfg.Platform, "-")
	asset := sharedrelease.MotherAssetName(goos, goarch)
	slog.Info("staging mother self-update", "version", row.DesiredVersion, "attempt", attempt)

	if err := os.MkdirAll(u.cfg.StageDir, 0o755); err != nil {
		return u.record(err)
	}
	if err := selfupdate.Place(u.client, u.cfg.ReleaseBaseURL, row.DesiredVersion, asset, u.StagedPath()); err != nil {
		return u.record(err)
	}
	if err := u.st.StageMotherUpdate(row.DesiredVersion); err != nil {
		return err
	}

	slog.Info("mother self-update staged; shutting down for the promote step",
		"version", row.DesiredVersion, "staged", u.StagedPath())
	u.shutdown()
	return nil
}

// record stores the reason and returns the original error, so the caller's log
// and the panel see the same thing.
func (u *Updater) record(err error) error {
	if storeErr := u.st.RecordMotherUpdateError(err.Error()); storeErr != nil {
		slog.Error("recording a self-update failure failed too", "err", storeErr)
	}
	return err
}

func lastReason(msg string) string {
	if msg == "" {
		return "no reason recorded"
	}
	return msg
}

// Run reconciles once, then converges on a ticker until ctx is done.
//
// The interval is one indexed read of a single row against a database this
// process already holds open — cheaper than the hourly retention sweep — and
// it bounds "I clicked update" to well under a minute.
func (u *Updater) Run(ctx context.Context, running string) {
	if err := u.Reconcile(running); err != nil {
		slog.Error("reconciling the mother's update state", "err", err)
	}
	ticker := time.NewTicker(u.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := u.Tick(running); err != nil {
				slog.Error("mother self-update", "err", err)
			}
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./mother/selfupdate/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add mother/selfupdate/
git commit -m "feat(mother): converge on the version the panel asked for"
```

---

### Task 7: The API surface

**Files:**
- Create: `mother/api/mother.go`
- Create: `mother/api/mother_test.go`
- Modify: `mother/api/versions.go`
- Modify: `mother/api/versions_test.go`
- Modify: `mother/api/api.go` (wherever `API`, `New` and `Handler` live — the file that already holds `registerVersions`' caller)

**Interfaces:**
- Consumes: `store` writers (Task 4), `release.Snapshot.Mother` (Task 3), `Updater.Supported()`/`Platform()` (Task 6).
- Produces: `GET /api/version` extended; `PUT /api/mother/version`; `api.(*API).SetMotherUpdate(t MotherUpdateTarget)`.

- [ ] **Step 1: Write the failing tests**

Create `mother/api/mother_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubTarget stands in for the updater: the API only needs to know whether
// this deployment can promote, and what it is.
type stubTarget struct {
	supported bool
	platform  string
}

func (s stubTarget) Supported() bool  { return s.supported }
func (s stubTarget) Platform() string { return s.platform }

func TestSetMotherVersionAcceptsAPublishedBuildForThisPlatform(t *testing.T) {
	a, st := testAPIWithMotherBuilds(t, stubTarget{supported: true, platform: "linux-amd64"})

	res := doJSON(t, a, http.MethodPut, "/api/mother/version", `{"version":"v1.4.0"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "v1.4.0" {
		t.Fatalf("row: %+v", row)
	}
}

func TestSetMotherVersionRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  stubTarget
		payload string
		want    string
	}{
		{"the moving alias", stubTarget{true, "linux-amd64"}, `{"version":"latest"}`, "moving alias"},
		{"an unpublished version", stubTarget{true, "linux-amd64"}, `{"version":"v9.9.9"}`, "no published release"},
		{"a platform with no build", stubTarget{true, "linux-arm64"}, `{"version":"v1.4.0"}`, "no v1.4.0 mother build for linux-arm64"},
		{"a deployment that cannot promote", stubTarget{false, "linux-amd64"}, `{"version":"v1.4.0"}`, "not available on this deployment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := testAPIWithMotherBuilds(t, tc.target)
			res := doJSON(t, a, http.MethodPut, "/api/mother/version", tc.payload)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", res.Code, res.Body)
			}
			if !strings.Contains(res.Body.String(), tc.want) {
				t.Fatalf("body %s does not explain the refusal (%q)", res.Body, tc.want)
			}
			row, err := st.MotherUpdate()
			if err != nil {
				t.Fatal(err)
			}
			if row.DesiredVersion != "" {
				t.Fatalf("a refused target was written anyway: %+v", row)
			}
		})
	}
}

// Cancelling is how an operator stops a rollout that has not landed, and it
// must not be validated against the release list — the whole point is that the
// target may be one the mother can no longer reach.
func TestSetMotherVersionAcceptsAnEmptyVersionAsCancellation(t *testing.T) {
	a, st := testAPIWithMotherBuilds(t, stubTarget{supported: true, platform: "linux-amd64"})
	if err := st.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}

	res := doJSON(t, a, http.MethodPut, "/api/mother/version", `{"version":""}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "" {
		t.Fatalf("row: %+v", row)
	}
}

func TestGetVersionReportsTheMotherUpdateState(t *testing.T) {
	a, st := testAPIWithMotherBuilds(t, stubTarget{supported: true, platform: "linux-amd64"})

	res := doJSON(t, a, http.MethodGet, "/api/version", "")
	if !strings.Contains(res.Body.String(), `"mother_update_state":"idle"`) {
		t.Fatalf("idle state missing: %s", res.Body)
	}
	if !strings.Contains(res.Body.String(), `"mother_platform":"linux-amd64"`) {
		t.Fatalf("platform missing: %s", res.Body)
	}

	if err := st.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	res = doJSON(t, a, http.MethodGet, "/api/version", "")
	if !strings.Contains(res.Body.String(), `"mother_update_state":"pending"`) {
		t.Fatalf("pending state missing: %s", res.Body)
	}

	if err := st.FailMotherUpdate("gave up"); err != nil {
		t.Fatal(err)
	}
	res = doJSON(t, a, http.MethodGet, "/api/version", "")
	if !strings.Contains(res.Body.String(), `"mother_update_state":"failed"`) {
		t.Fatalf("failed state missing: %s", res.Body)
	}
}

func TestGetVersionReportsUnsupportedWhereItIs(t *testing.T) {
	a, _ := testAPIWithMotherBuilds(t, stubTarget{supported: false, platform: "linux-amd64"})
	res := doJSON(t, a, http.MethodGet, "/api/version", "")
	if !strings.Contains(res.Body.String(), `"mother_update_state":"unsupported"`) {
		t.Fatalf("body: %s", res.Body)
	}
}

// A mother with no updater wired at all (tests, and any embedding that never
// calls SetMotherUpdate) must read as unsupported rather than panic.
func TestGetVersionWithoutAnUpdaterIsUnsupported(t *testing.T) {
	a, _ := testAPIWithMotherBuilds(t, nil)
	res := doJSON(t, a, http.MethodGet, "/api/version", "")
	if !strings.Contains(res.Body.String(), `"mother_update_state":"unsupported"`) {
		t.Fatalf("body: %s", res.Body)
	}
}
```

Add the two helpers this file needs, adapting them to the package's existing test construction (read `mother/api/versions_test.go` first and reuse its store/cache setup verbatim):

```go
// testAPIWithMotherBuilds builds an API whose release index offers v1.4.0 for
// linux-amd64 only, so a target for any other platform is a real rejection.
// Pass a nil target to leave the updater unwired.
func testAPIWithMotherBuilds(t *testing.T, target MotherUpdateTarget) (*API, *store.Store) { ... }

// doJSON drives one authenticated request through the real handler chain.
func doJSON(t *testing.T, a *API, method, path, body string) *httptest.ResponseRecorder { ... }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./mother/api/`
Expected: FAIL — `undefined: MotherUpdateTarget`, 404 on `/api/mother/version`.

- [ ] **Step 3: Implement**

Create `mother/api/mother.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/osman-yahya/feast-watch/mother/release"
)

// MotherUpdateTarget is what the API needs to know about the process it is
// retargeting: whether this deployment can actually promote a staged binary,
// and which platform's build it would need. Narrow on purpose — the API has no
// business driving the updater, only describing and arming it.
type MotherUpdateTarget interface {
	Supported() bool
	Platform() string
}

// SetMotherUpdate wires the updater in. Left unset — in tests, and in any
// deployment that never constructs one — the mother reports `unsupported` and
// refuses targets, which is the honest answer rather than a panic.
func (a *API) SetMotherUpdate(t MotherUpdateTarget) { a.motherUpdate = t }

func (a *API) registerMother(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/mother/version", a.requireAPIKey(a.handleSetMotherVersion))
}

func (a *API) motherUpdateSupported() bool {
	return a.motherUpdate != nil && a.motherUpdate.Supported()
}

func (a *API) motherPlatform() string {
	if a.motherUpdate == nil {
		return ""
	}
	return a.motherUpdate.Platform()
}

func (a *API) handleSetMotherVersion(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "malformed payload")
		return
	}
	in.Version = strings.TrimSpace(in.Version)

	// Empty clears the target, and is deliberately not validated: cancelling a
	// rollout must work even when the version being cancelled is one the
	// release index no longer offers.
	if in.Version != "" {
		if !a.motherUpdateSupported() {
			writeJSON(w, http.StatusBadRequest, nil, "self-update is not available on this deployment")
			return
		}
		if msg := rejectMotherVersion(in.Version, a.motherPlatform(), a.releases.Snapshot().Mother); msg != "" {
			writeJSON(w, http.StatusBadRequest, nil, msg)
			return
		}
	}

	if err := a.st.SetMotherDesiredVersion(in.Version, time.Now().Unix()); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"desired_version": in.Version}, "")
}

// rejectMotherVersion returns why this version cannot be the mother's target,
// or "" when it can. It mirrors rejectVersion for agents — same rules, same
// wording — so "publishable" means one thing across the system.
func rejectMotherVersion(ver, platform string, builds []release.Build) string {
	if ver == latestAlias {
		return "latest is a moving alias, not a version; pick a concrete version"
	}
	for _, b := range builds {
		if b.Version != ver {
			continue
		}
		for _, p := range b.Platforms {
			if p == platform {
				return ""
			}
		}
		return "no " + ver + " mother build for " + platform
	}
	return "version " + ver + " has no published release"
}
```

Add the field to the `API` struct and call `registerMother(mux)` next to `registerVersions(mux)` in `Handler()`:

```go
	// motherUpdate is nil until SetMotherUpdate is called; every read goes
	// through motherUpdateSupported/motherPlatform, which treat nil as "this
	// deployment cannot update itself".
	motherUpdate MotherUpdateTarget
```

Extend `mother/api/versions.go`:

```go
type versionView struct {
	MotherVersion string          `json:"mother_version"`
	Agents        []release.Build `json:"agents"`
	CheckedAt     time.Time       `json:"checked_at"`
	Stale         bool            `json:"stale"`

	// The mother's own rollout: what it could become, what it was told to
	// become, and how that is going. MotherPlatform is what MotherBuilds must
	// carry for a version to be selectable, and is "" where self-update is
	// not available at all.
	MotherBuilds         []release.Build `json:"mother_builds"`
	MotherPlatform       string          `json:"mother_platform"`
	MotherDesiredVersion string          `json:"mother_desired_version"`
	MotherUpdateState    string          `json:"mother_update_state"`
	MotherUpdateError    string          `json:"mother_update_error"`
}

func (a *API) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	snap := a.releases.Snapshot()
	row, err := a.st.MotherUpdate()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, versionView{
		MotherVersion: version.Version,
		Agents:        snap.Builds,
		CheckedAt:     snap.CheckedAt,
		Stale:         snap.Stale,

		MotherBuilds:         snap.Mother,
		MotherPlatform:       a.motherPlatform(),
		MotherDesiredVersion: row.DesiredVersion,
		MotherUpdateState:    motherUpdateState(row, version.Version, a.motherUpdateSupported()),
		MotherUpdateError:    row.Error,
	}, "")
}

// motherUpdateState is a projection of the stored row, computed the way
// updateState computes an agent's, so the panel never re-implements the rule.
//
// `unsupported` outranks everything: on a deployment that cannot promote, no
// other state is actionable, and rendering `idle` there would offer a control
// that could only disappoint.
func motherUpdateState(row store.MotherUpdate, running string, supported bool) string {
	if !supported {
		return "unsupported"
	}
	if row.Error != "" {
		return "failed"
	}
	if row.DesiredVersion == "" || row.DesiredVersion == running {
		return "idle"
	}
	return "pending"
}
```

Note the ordering difference from `updateState`: `failed` is checked before `idle` because `FailMotherUpdate` clears the target and keeps the error, so a failed mother update has no desired version left to test.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./mother/...`
Expected: PASS, including the existing `versions_test.go`.

- [ ] **Step 5: Commit**

```bash
git add mother/api/
git commit -m "feat(api): target the mother's own version from the panel"
```

---

### Task 8: Wire it into the process

**Files:**
- Modify: `mother/cmd/feast-watch/main.go`

**Interfaces:**
- Consumes: `selfupdate.New/Run` (Task 6), `api.SetMotherUpdate` (Task 7).
- Produces: env vars `FW_MOTHER_UPDATE_INTERVAL` (default `30s`), `FW_MOTHER_PROMOTE_PATH` (default `/usr/local/sbin/feast-watch-mother-promote`), `FW_MOTHER_STAGE_DIR` (default `<dir of FW_DB_PATH>/update`).

- [ ] **Step 1: Add the wiring**

After the release cache is constructed and before `a := api.New(...)`:

```go
	// The mother's own rollout. `stop` is the same cancel the signal handler
	// uses, so a staged update leaves through the graceful shutdown path
	// rather than by exiting from under an in-flight ingest — this process is
	// the single writer of a SQLite database and must close it cleanly.
	updater := selfupdate.New(st, selfupdate.Config{
		ReleaseBaseURL: env("FW_RELEASE_BASE_URL", sharedrelease.DefaultBaseURL),
		PromotePath:    env("FW_MOTHER_PROMOTE_PATH", "/usr/local/sbin/feast-watch-mother-promote"),
		StageDir:       env("FW_MOTHER_STAGE_DIR", filepath.Join(filepath.Dir(dbPath), "update")),
		Platform:       runtime.GOOS + "-" + runtime.GOARCH,
		MaxAttempts:    3,
		Interval:       motherUpdateInterval(),
	}, &http.Client{Timeout: 60 * time.Second}, time.Now, stop)
```

`dbPath` is the value already passed to `store.Open`; hoist it into a variable if it is currently inline. Add the parse helper beside `releasePollInterval`, following its shape exactly:

```go
// defaultMotherUpdateInterval is how often the mother checks its own rollout
// target. One indexed read of a single row against a database this process
// already holds open — cheaper than the hourly retention sweep.
const defaultMotherUpdateInterval = 30 * time.Second

func motherUpdateInterval() time.Duration {
	raw := os.Getenv("FW_MOTHER_UPDATE_INTERVAL")
	if raw == "" {
		return defaultMotherUpdateInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("FW_MOTHER_UPDATE_INTERVAL is not a positive duration; using the default",
			"value", raw, "default", defaultMotherUpdateInterval)
		return defaultMotherUpdateInterval
	}
	return d
}
```

After `a := api.New(st, apiKey, releases)`:

```go
	a.SetMotherUpdate(updater)
	go updater.Run(ctx, version.Version)
	if !updater.Supported() {
		slog.Info("mother self-update is unavailable: no promote helper on this deployment",
			"looked_for", env("FW_MOTHER_PROMOTE_PATH", "/usr/local/sbin/feast-watch-mother-promote"))
	}
```

Add imports: `path/filepath`, `runtime`, `github.com/osman-yahya/feast-watch/mother/selfupdate`, `github.com/osman-yahya/feast-watch/shared/version`.

- [ ] **Step 2: Verify it builds and the process still starts**

Run:
```bash
go build ./... && go vet ./...
FW_DB_PATH=/tmp/wire.db FW_API_KEY=k FW_PUBLIC_URL=http://127.0.0.1:8443 \
  FW_MOTHER_PROMOTE_PATH=/nonexistent timeout 3 go run ./mother/cmd/feast-watch 2>&1 | head -5
```
Expected: build and vet clean; the log carries `mother self-update is unavailable` and then `mother listening`.

- [ ] **Step 3: Run the smoke suite**

Run: `./e2e/local_smoke.sh`
Expected: all checks pass.

- [ ] **Step 4: Commit**

```bash
git add mother/cmd/feast-watch/main.go
git commit -m "feat(mother): run the self-update loop alongside the server"
```

---

### Task 9: The promote helper and the unit

**Files:**
- Create: `deploy/feast-watch-mother-promote`
- Create: `e2e/promote_test.sh`
- Modify: `deploy/feast-watch-mother.service`
- Modify: `deploy/mother-install.sh`
- Modify: `deploy/mother-uninstall.sh`

**Interfaces:**
- Consumes: the staged path convention `<state>/update/feast-watch.new` (Task 6).
- Produces: `/usr/local/sbin/feast-watch-mother-promote`, and `promote=`/`staged=`/`backup=` keys in `/etc/feast-watch/mother-manifest`.

- [ ] **Step 1: Write the failing test**

Create `e2e/promote_test.sh`, following `e2e/colocation_test.sh`'s `FW_ROOT` style so it needs neither root nor systemd:

```bash
#!/usr/bin/env bash
# The promote helper: the root half of the mother's self-update.
#
# Runs against a temp tree via FW_ROOT, so it needs neither root nor systemd.
set -euo pipefail

cd "$(dirname "$0")/.."

ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT

pass() { echo "  ok   $1"; }
fail() { echo "  FAIL $1" >&2; exit 1; }
step() { echo; echo "== $1"; }

BIN="$ROOT/usr/local/bin/feast-watch"
STAGED="$ROOT/var/lib/feast-watch/update/feast-watch.new"
mkdir -p "$(dirname "$BIN")" "$(dirname "$STAGED")"

step "1. nothing staged is a no-op, not a failure"
printf 'old' > "$BIN"
FW_ROOT="$ROOT" bash deploy/feast-watch-mother-promote ||
  fail "the helper failed with nothing staged — it runs on EVERY start"
[ "$(cat "$BIN")" = "old" ] || fail "the binary changed with nothing staged"
pass "no-op with nothing staged"

step "2. a staged binary is installed, and the old one kept"
printf 'new' > "$STAGED"
chmod 0755 "$STAGED"
FW_ROOT="$ROOT" bash deploy/feast-watch-mother-promote || fail "promote failed"
[ "$(cat "$BIN")" = "new" ] || fail "the staged binary was not installed"
[ "$(cat "$BIN.bak")" = "old" ] || fail "the previous binary was not kept as .bak"
[ -x "$BIN" ] || fail "the installed binary is not executable"
[ ! -e "$STAGED" ] || fail "the staged file was left behind — it would be promoted again"
pass "staged binary promoted, previous kept as .bak"

step "3. running again with nothing staged changes nothing"
FW_ROOT="$ROOT" bash deploy/feast-watch-mother-promote || fail "second run failed"
[ "$(cat "$BIN")" = "new" ] || fail "a second run altered the binary"
pass "idempotent"

step "4. an empty staged file is refused"
: > "$STAGED"
chmod 0755 "$STAGED"
FW_ROOT="$ROOT" bash deploy/feast-watch-mother-promote || fail "the helper must not fail the boot"
[ "$(cat "$BIN")" = "new" ] || fail "an empty staged file replaced the binary"
[ ! -e "$STAGED" ] || fail "the refused staged file was left to be retried forever"
pass "empty staged file refused and cleared"

echo
echo "all checks passed"
```

- [ ] **Step 2: Run it to verify it fails**

Run: `bash e2e/promote_test.sh`
Expected: FAIL — `deploy/feast-watch-mother-promote` does not exist.

- [ ] **Step 3: Write the helper**

Create `deploy/feast-watch-mother-promote`:

```bash
#!/usr/bin/env bash
# Install a staged mother binary, as root, before the service starts.
#
# This is the privileged half of the mother's self-update. The mother runs as
# an unprivileged user under ProtectSystem=strict, so /usr/local/bin is
# read-only inside its namespace whatever the file permissions say: it can
# download and verify a new binary, but it cannot install one. It stages the
# verified file inside its StateDirectory and exits; systemd restarts the unit
# and runs this from ExecStartPre with a `+` prefix, which means as root and
# outside the sandbox.
#
# It runs on EVERY start, including the ordinary ones. That is deliberate — it
# is what makes a half-finished update impossible: either a staged file is
# there and becomes the binary, or there is nothing to do.
#
# It never fails the boot. A mother that cannot be upgraded must still start;
# the running mother reports the failure to the panel, which a dead one cannot.
set -uo pipefail

# FW_ROOT prefixes every path. Empty in production; e2e/promote_test.sh sets it
# to a temp tree so this can be exercised without being root.
FW_ROOT="${FW_ROOT:-}"

BIN="$FW_ROOT/usr/local/bin/feast-watch"
STAGED="$FW_ROOT/var/lib/feast-watch/update/feast-watch.new"
BACKUP="$BIN.bak"

[ -f "$STAGED" ] || exit 0

# An empty or unreadable staged file is not a binary. Clearing it rather than
# leaving it is what stops it being retried on every start forever.
if [ ! -s "$STAGED" ]; then
  echo "feast-watch promote: staged file is empty; discarding" >&2
  rm -f "$STAGED"
  exit 0
fi

if [ -f "$BIN" ]; then
  # Kept for a manual rollback: if the new binary will not start, systemd will
  # restart it forever and the panel is down. This is the way back.
  cp -p "$BIN" "$BACKUP" || echo "feast-watch promote: could not keep a backup" >&2
fi

if install -m 0755 "$STAGED" "$BIN"; then
  echo "feast-watch promote: installed the staged binary"
  rm -f "$STAGED"
else
  echo "feast-watch promote: could not install the staged binary" >&2
  # Deliberately left in place: the mother reports "staged but not running" on
  # its next boot, and the attempt bound stops it after three.
fi

exit 0
```

`chmod +x deploy/feast-watch-mother-promote`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `bash e2e/promote_test.sh && shellcheck deploy/feast-watch-mother-promote e2e/promote_test.sh`
Expected: all checks pass; shellcheck silent.

- [ ] **Step 5: Hook it into the unit and the installer**

In `deploy/feast-watch-mother.service`, above `ExecStart`:

```ini
# The root half of the self-update. `+` runs it outside the sandbox below, as
# root, which is the only way a staged binary can reach /usr/local/bin — this
# unit's own hardening makes that path read-only to the service. It runs on
# every start and does nothing when nothing is staged.
ExecStartPre=+/usr/local/sbin/feast-watch-mother-promote
```

In `deploy/mother-install.sh`, add beside the other destinations:

```bash
PROMOTE_DEST="$FW_ROOT/usr/local/sbin/feast-watch-mother-promote"
```

Add a function and call it from `main` right after the binary is installed:

```bash
# install_promote_helper puts the root half of the self-update on disk.
#
# Without it the mother reports `unsupported` and the panel's update control is
# disabled — which is honest, but this is the deployment where self-update is
# meant to work, so the helper belongs in the same install that creates the
# unit referencing it.
install_promote_helper() {
  local src
  src="$(dirname "$0")/feast-watch-mother-promote"
  [ -f "$src" ] || { echo "promote helper not found at $src" >&2; return 1; }

  echo "-> installing promote helper"
  mkdir -p "$(dirname "$PROMOTE_DEST")"
  install -m 0755 "$src" "$PROMOTE_DEST"
}
```

Extend `write_manifest` with the three paths the self-update creates:

```bash
    echo "promote=$PROMOTE_DEST"
    echo "staged=/var/lib/feast-watch/update"
    echo "backup=$BIN_DEST.bak"
```

In `deploy/mother-uninstall.sh`, remove all three alongside the existing removals, following the file's existing removal helper — a file created outside what the uninstaller reads is a file nothing cleans.

- [ ] **Step 6: Verify the installers still agree**

Run: `bash e2e/colocation_test.sh && bash e2e/promote_test.sh && shellcheck bin/release.sh e2e/*.sh deploy/*.sh deploy/feast-watch-mother-promote mother/api/uninstall.sh`
Expected: all pass, shellcheck silent.

- [ ] **Step 7: Commit**

```bash
git add deploy/ e2e/promote_test.sh
git commit -m "feat(deploy): promote a staged mother binary as root at start"
```

---

### Task 10: Document the upgrade

**Files:**
- Modify: `QUICKSTART.md`

- [ ] **Step 1: Add a "Upgrading the mother" section** after the release section

```markdown
### Upgrading the mother

From the panel: open the Mother card on the monitoring page, pick a published
version, confirm. The mother downloads that build from GitHub Releases, verifies
its SHA-256, stages it in `/var/lib/feast-watch/update/`, and shuts down.
systemd restarts it, `ExecStartPre=+/usr/local/sbin/feast-watch-mother-promote`
installs the staged binary as root, and the new one starts. The panel is
unreachable for a few seconds in the middle; while a target is pending it shows
that rather than an error.

A target is tried at most three times, counted across restarts. After that the
mother drops the target and leaves the reason on the card — it does not keep
downloading and exiting.

**Where this does not work.** In Docker there is no systemd and no promote
helper, so the image does not ship one: the mother reports `unsupported` and
refuses a target instead of restarting into the same version. Upgrade a
containerised mother by building a new image.

**By hand**, if the panel is not available:

```bash
bin/release.sh --mother-only
sudo deploy/mother-install.sh bin/build/feast-watch
```

The installer restarts an already-running unit, so the new binary is what ends
up running. It used to only `systemctl start`, which is a no-op on an active
unit — the upgrade appeared to succeed while the old process kept running from
the unlinked inode, and the panel kept reporting the old version.

**Rolling back a mother that will not start.** The promote helper keeps the
previous binary:

```bash
sudo systemctl stop feast-watch-mother
sudo mv /usr/local/bin/feast-watch.bak /usr/local/bin/feast-watch
sudo systemctl start feast-watch-mother
```
```

- [ ] **Step 2: Commit**

```bash
git add QUICKSTART.md
git commit -m "docs: how a mother is upgraded, and how to roll one back"
```

---

### Task 11: Proxy the endpoint

**Files (repo `/Users/ceydaakin/feast/feast-mobile-backend`):**
- Modify: `app/api/admin/monitoring_routes.go`
- Modify: `app/api/admin/monitoring.go`
- Modify: `app/api/admin/monitoring_test.go`

**Interfaces:**
- Consumes: mother's `PUT /api/mother/version` (Task 7).
- Produces: `PUT /admin/monitoring/mother/version`, gated by `role.PermSystemAgentUpdate`.

- [ ] **Step 1: Write the failing test**

Append to `app/api/admin/monitoring_test.go`, copying the construction of whichever existing test drives `MonitoringSetServerVersion` — reuse its fake mother server and handler setup verbatim:

```go
// Retargeting the mother replaces the binary that runs the monitoring, so it
// rides the same write permission as an agent rollout and forwards mother's
// refusal verbatim: the reason is the whole value of the 400.
func TestMonitoringSetMotherVersionForwardsPayloadAndErrors(t *testing.T) {
	var gotPath, gotBody string
	mother := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"success":false,"data":null,"error":"no v1.4.0 mother build for linux-arm64"}`))
	}))
	defer mother.Close()

	h := testHandler(t, mother.URL)
	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/monitoring/mother/version",
		strings.NewReader(`{"version":"v1.4.0"}`))
	h.MonitoringSetMotherVersion(res, req)

	if gotPath != "/api/mother/version" {
		t.Fatalf("proxied to %q", gotPath)
	}
	if gotBody != `{"version":"v1.4.0"}` {
		t.Fatalf("body %q was not forwarded verbatim", gotBody)
	}
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "no v1.4.0 mother build for linux-arm64") {
		t.Fatalf("mother's reason was lost: %s", res.Body)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./app/api/admin/ -run MotherVersion`
Expected: FAIL — `h.MonitoringSetMotherVersion` undefined.

- [ ] **Step 3: Implement**

In `monitoring_routes.go`, beside the other version routes:

```go
		// Retargeting the mother itself. system:agent_update, not
		// system:health: this replaces the binary that runs the monitoring,
		// which is further from moderation than an agent rollout, not closer.
		// "mother" is a distinct static segment and shadows nothing in the
		// servers/ or groups/ families.
		{http.MethodPut, "/admin/monitoring/mother/version", role.PermSystemAgentUpdate, h.MonitoringSetMotherVersion},
```

In `monitoring.go`, add the handler modelled exactly on `MonitoringSetServerVersion` — same forwarding helper, same error mapping, targeting `/api/mother/version` with no path parameter to interpolate. Read that neighbour and mirror it rather than inventing a second style.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./app/api/admin/`
Expected: PASS, including the route-gate table test, which now drives the new row through the real middleware chain.

- [ ] **Step 5: Commit**

```bash
git add app/api/admin/
git commit -m "feat(monitoring): proxy the mother version endpoint"
```

---

### Task 12: The panel's Mother card

**Files (repo `/Users/ceydaakin/GitHub/feast-mobile-backend-control`):**
- Modify: `src/api/monitoring.js`
- Modify: `src/api/monitoring.test.js`
- Create: `src/features/monitoring/UpdateMotherDialog.jsx`
- Create: `src/features/monitoring/UpdateMotherDialog.test.jsx`
- Create: `src/features/monitoring/MotherCard.jsx`
- Create: `src/features/monitoring/MotherCard.test.jsx`
- Modify: `src/features/monitoring/MonitoringPage.jsx`

**Interfaces:**
- Consumes: `GET /admin/monitoring/version` fields `mother_builds`, `mother_platform`, `mother_desired_version`, `mother_update_state`, `mother_update_error` (Task 7); `PUT /admin/monitoring/mother/version` (Task 11).
- Produces: `setMotherVersion(version)`; `<MotherCard version={versionQuery.data} canUpdate={canUpdate} />`.

- [ ] **Step 1: Write the failing API-client test**

Append to `src/api/monitoring.test.js`, matching the file's existing `apiClient` mocking style:

```js
describe('setMotherVersion', () => {
  it('puts the target to the mother route', async () => {
    apiClient.put.mockResolvedValue({ data: { desired_version: 'v1.4.0' } })
    await setMotherVersion('v1.4.0')
    expect(apiClient.put).toHaveBeenCalledWith('/admin/monitoring/mother/version', {
      version: 'v1.4.0',
    })
  })

  // Cancelling must reach the mother unvalidated: the whole point is that the
  // version being cancelled may be one the release index no longer offers.
  it('sends an empty version to cancel', async () => {
    apiClient.put.mockResolvedValue({ data: { desired_version: '' } })
    await setMotherVersion('')
    expect(apiClient.put).toHaveBeenCalledWith('/admin/monitoring/mother/version', { version: '' })
  })
})
```

- [ ] **Step 2: Run it to verify it fails**

Run: `npx vitest run src/api/monitoring.test.js`
Expected: FAIL — `setMotherVersion is not a function`.

- [ ] **Step 3: Add the client function**

In `src/api/monitoring.js`, beside `setServerVersion`:

```js
// Points the mother at a version of itself; it converges within a minute and
// restarts to apply it, so the panel is briefly unreachable. Pass an empty
// string to cancel a target that has not landed.
//
// Mother rejects the "latest" alias, an unpublished version, a version with no
// mother build for its own platform, and — on a deployment with no promote
// helper, such as Docker — any target at all. Each arrives as a 400 whose
// message the caller must surface.
export async function setMotherVersion(version) {
  const response = await apiClient.put('/admin/monitoring/mother/version', { version })
  return response.data
}
```

- [ ] **Step 4: Write the failing card test**

Create `src/features/monitoring/MotherCard.test.jsx`:

```jsx
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { MotherCard } from './MotherCard'

function renderCard(version, canUpdate = true) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <MotherCard version={version} canUpdate={canUpdate} />
    </QueryClientProvider>,
  )
}

const IDLE = {
  mother_version: 'v1.3.0',
  mother_builds: [{ version: 'v1.4.0', platforms: ['linux-amd64'] }],
  mother_platform: 'linux-amd64',
  mother_desired_version: '',
  mother_update_state: 'idle',
  mother_update_error: '',
}

it('shows the running version', () => {
  renderCard(IDLE)
  expect(screen.getByText('v1.3.0')).toBeInTheDocument()
})

// Docker has no promote hook, so an update there would restart into the same
// version. The control says why instead of pretending it would work.
it('disables the control where self-update is unavailable', () => {
  renderCard({ ...IDLE, mother_update_state: 'unsupported' })
  expect(screen.getByRole('button', { name: /güncelle/i })).toBeDisabled()
  expect(screen.getByText(/bu kurulumda/i)).toBeInTheDocument()
})

// The mother is offline while it applies, so it cannot report its own
// progress: a pending target plus an unreachable API is the expected state,
// not an error.
it('reports a pending target as a restart in progress', () => {
  renderCard({ ...IDLE, mother_desired_version: 'v1.4.0', mother_update_state: 'pending' })
  expect(screen.getByText(/yeniden başlatılıyor/i)).toBeInTheDocument()
})

it('surfaces the reason a target was given up on', () => {
  renderCard({
    ...IDLE,
    mother_update_state: 'failed',
    mother_update_error: 'gave up on v1.4.0 after 3 attempts: checksum mismatch',
  })
  expect(screen.getByText(/checksum mismatch/)).toBeInTheDocument()
})

it('hides the control without the update permission', () => {
  renderCard(IDLE, false)
  expect(screen.queryByRole('button', { name: /güncelle/i })).not.toBeInTheDocument()
})
```

- [ ] **Step 5: Run it to verify it fails**

Run: `npx vitest run src/features/monitoring/MotherCard.test.jsx`
Expected: FAIL — cannot resolve `./MotherCard`.

- [ ] **Step 6: Build the card and the dialog**

`MotherCard.jsx` renders `version.mother_version`, a state line, the error when present, and a "Güncelle" button that opens `UpdateMotherDialog`. Copy the card's markup conventions from `FleetSummary.jsx`; copy the dialog wholesale from `UpdateAgentDialog.jsx` and change exactly three things:

1. The option list is `version.mother_builds` narrowed to builds whose `platforms` include `version.mother_platform`.
2. The mutation is `setMotherVersion(target)` and invalidates `['monitoring', 'version']`.
3. The description says the mother will restart and the panel will be briefly unreachable.

Keep `updatePolicy`'s downgrade confirmation (`needsConfirmation`, `DOWNGRADE_CONFIRM_MS`, `relationWarning`) — comparing `mother_version` against the selected target — and `versionListStatus` for the empty/stale distinction. The Turkish copy the tests pin:

- unsupported: `Bu kurulumda mother kendini güncelleyemez (promote betiği yok).`
- pending: `Yeni sürüm uygulanıyor, mother yeniden başlatılıyor…`

- [ ] **Step 7: Mount it on the page**

In `MonitoringPage.jsx`, render `<MotherCard version={versionQuery.data} canUpdate={canUpdate} />` beside `FleetSummary`, only when `versionQuery.data` exists — before the query resolves there is nothing to describe, and judging an absent payload is what made the release-list line flash a false "no version" on load.

- [ ] **Step 8: Run the whole suite**

Run: `npx vitest run && npx eslint src`
Expected: every test passes; eslint clean.

- [ ] **Step 9: Commit**

```bash
git add src/api/monitoring.js src/api/monitoring.test.js src/features/monitoring/
git commit -m "feat(monitoring): update the mother from the panel"
```

---

## Verification

After Task 12, from `feast-watch`:

```bash
go vet ./... && gofmt -l . && go test -race -cover ./...
shellcheck bin/release.sh e2e/*.sh deploy/*.sh deploy/feast-watch-mother-promote mother/api/uninstall.sh
./e2e/local_smoke.sh && bash e2e/colocation_test.sh && bash e2e/promote_test.sh
```

Then the end-to-end rehearsal, which is the only thing that proves the promote hook: on a throwaway Linux host (or a systemd container), install with `deploy/mother-install.sh`, publish a tag, set the mother's target from the panel, and watch `journalctl -u feast-watch-mother` show `staging mother self-update` → `feast-watch promote: installed the staged binary` → the new version in the panel header.

## Open questions carried from the spec

These are the owner's to answer; none blocks Task 1–12, and each would be a small follow-up:

1. Should the mother's downgrade confirmation be stronger than the agent's 5 seconds, given a bad mother version takes the panel with it?
2. Should replacing the control plane need a permission of its own rather than `system:agent_update`?
3. `v1.0.0` names three different binaries because the tag was moved three times. Should the mother rollout refuse that version explicitly?
