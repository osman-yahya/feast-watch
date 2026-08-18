package release

import (
	"context"
	"sync"
	"time"
)

// Source is what the cache reads from; the GitHub Client implements it.
type Source interface {
	Fetch(context.Context) ([]Build, bool, error)
}

// Snapshot is an immutable view of the release index, safe to hand to a
// request handler.
type Snapshot struct {
	Builds    []Build   `json:"builds"`
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
		snap: Snapshot{Builds: []Build{}, Stale: true},
	}
}

// Seed publishes a build list supplied by configuration, so a mother with no
// route to github.com is still usable. A successful fetch replaces it.
func (c *Cache) Seed(builds []Build) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = Snapshot{Builds: cloneBuilds(builds), CheckedAt: c.now(), Stale: false}
}

// Refresh reads the source and republishes. A failure is returned to the
// caller and marks the snapshot stale, but never empties it: GitHub being
// unreachable is exactly when an operator most needs the list they had.
func (c *Cache) Refresh(ctx context.Context) error {
	builds, notModified, err := c.src.Fetch(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.snap = Snapshot{Builds: c.snap.Builds, CheckedAt: c.snap.CheckedAt, Stale: true}
		return err
	}
	if notModified {
		c.snap = Snapshot{Builds: c.snap.Builds, CheckedAt: c.now(), Stale: false}
		return nil
	}
	c.snap = Snapshot{Builds: cloneBuilds(builds), CheckedAt: c.now(), Stale: false}
	return nil
}

// Snapshot returns a copy. Handing out the cache's own slices would let a
// handler's incidental write reach shared state.
func (c *Cache) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Snapshot{
		Builds:    cloneBuilds(c.snap.Builds),
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
