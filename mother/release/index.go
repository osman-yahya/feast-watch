// Package release keeps the mother's view of what builds exist — the agents'
// and its own.
//
// The list used to be read from the GitHub API. It is now read from the
// catalogue the mother compiled into (mother/build), because the mother builds
// everything its fleet runs and nothing is published anywhere else. What stays
// is the shape: an immutable snapshot, so naming a rollout target is a check
// against what is actually downloadable rather than against a version somebody
// remembers building.
package release

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Source is what the cache reads from; mother/build.Store is the implementation
// and, since the mother stopped asking anything else what versions exist, the
// only one. The interface is kept because it is what lets the cache be tested
// without a filesystem. The two families come back apart rather than tagged in
// one list — see Build.
type Source interface {
	Fetch(context.Context) (agents, mother []Build, notModified bool, err error)
}

// Build is one downloadable version and the platforms it was built for. The
// family it belongs to is carried by which list it is in, not by a field: every
// consumer wants exactly one of them, and a Kind field would move the
// separation to each call site instead of settling it here.
type Build struct {
	Version   string   `json:"version"`
	Platforms []string `json:"platforms"`
}

// SortDescending puts the newest version first, which is the order a rollout
// dropdown reads in. Exported because the build catalogue orders its own list
// with it, and a second comparator is how two lists begin disagreeing about
// which build is newest.
func SortDescending(builds []Build) {
	sort.Slice(builds, func(i, j int) bool {
		return naturalLess(builds[j].Version, builds[i].Version)
	})
}

// Snapshot is an immutable view of the release index, safe to hand to a
// request handler.
type Snapshot struct {
	Builds []Build `json:"builds"`
	// Mother is the same list for the mother's own binary. Separate rather
	// than merged: the mother is built for fewer platforms and is targeted
	// through a different endpoint, so a merged list would make "which
	// versions exist" ambiguous at every read.
	Mother    []Build   `json:"mother"`
	CheckedAt time.Time `json:"checked_at"`
	// Stale reports that the last refresh failed, so these builds are the last
	// known good answer rather than a current one. It is surfaced rather than
	// hidden: an operator picking a rollout target should know the list may
	// have moved on.
	Stale bool `json:"stale"`
}

// Cache holds the current snapshot behind a read-write lock. Refresh replaces
// the whole snapshot; it never mutates the one readers may already hold.
type Cache struct {
	src Source
	now func() time.Time

	mu   sync.RWMutex
	snap Snapshot
}

func NewCache(src Source, now func() time.Time) *Cache {
	return &Cache{
		src:  src,
		now:  now,
		snap: Snapshot{Builds: []Build{}, Mother: []Build{}, Stale: true},
	}
}

// Refresh reads the source and republishes. A failure is returned to the
// caller and marks the snapshot stale, but never empties it: a catalogue that
// could not be read is exactly when an operator most needs the list they had.
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

// Snapshot returns a copy. Handing out the cache's own slices would let a
// handler's incidental write reach shared state.
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

// Poll refreshes on a ticker until ctx is done, reporting each failure to
// report. The first refresh happens immediately so a freshly started mother is
// usable without waiting out an interval.
func (c *Cache) Poll(ctx context.Context, every time.Duration, report func(error)) {
	refresh := func() {
		if err := c.Refresh(ctx); err != nil && report != nil {
			report(err)
		}
	}
	refresh()

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

func cloneBuilds(in []Build) []Build {
	out := make([]Build, len(in))
	for i, b := range in {
		out[i] = Build{Version: b.Version, Platforms: append([]string(nil), b.Platforms...)}
	}
	return out
}
