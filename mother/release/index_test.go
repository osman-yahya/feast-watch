package release

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeSource struct {
	calls       int
	builds      []Build
	mother      []Build
	notModified bool
	err         error
}

func (f *fakeSource) Fetch(context.Context) ([]Build, []Build, bool, error) {
	f.calls++
	return f.builds, f.mother, f.notModified, f.err
}

func at(sec int64) func() time.Time {
	return func() time.Time { return time.Unix(sec, 0).UTC() }
}

func TestCacheStartsEmptyAndStale(t *testing.T) {
	c := NewCache(&fakeSource{}, at(100))
	snap := c.Snapshot()
	if len(snap.Builds) != 0 || !snap.Stale {
		t.Fatalf("a cache that has never fetched must report stale: %+v", snap)
	}
}

func TestRefreshPublishesTheFetchedBuilds(t *testing.T) {
	src := &fakeSource{builds: []Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64"}}}}
	c := NewCache(src, at(100))

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := c.Snapshot()
	if len(snap.Builds) != 1 || snap.Builds[0].Version != "v1.4.0" {
		t.Fatalf("builds: %+v", snap.Builds)
	}
	if snap.Stale {
		t.Fatal("a fresh snapshot must not be stale")
	}
	if !snap.CheckedAt.Equal(time.Unix(100, 0).UTC()) {
		t.Fatalf("checkedAt: %v", snap.CheckedAt)
	}
}

// A 304 says the list is unchanged, which is a successful check — the snapshot
// keeps its contents and only its timestamp moves.
func TestRefreshKeepsBuildsWhenNotModified(t *testing.T) {
	src := &fakeSource{builds: []Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64"}}}}
	c := NewCache(src, at(100))
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	src.builds, src.notModified = nil, true
	c.now = at(200)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	snap := c.Snapshot()
	if len(snap.Builds) != 1 {
		t.Fatalf("a 304 emptied the index: %+v", snap.Builds)
	}
	if !snap.CheckedAt.Equal(time.Unix(200, 0).UTC()) {
		t.Fatalf("a successful check must move checkedAt: %v", snap.CheckedAt)
	}
}

// GitHub being unreachable must not erase what is known. Rolling a version
// back is exactly when the network is least trustworthy and the list is most
// needed, so the last good answer is kept and flagged stale.
func TestRefreshKeepsTheLastGoodIndexOnFailure(t *testing.T) {
	src := &fakeSource{builds: []Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64"}}}}
	c := NewCache(src, at(100))
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	src.err = errors.New("dial tcp: no route to host")
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("the failure must be reported to the caller")
	}

	snap := c.Snapshot()
	if len(snap.Builds) != 1 {
		t.Fatalf("a failed refresh emptied the index: %+v", snap.Builds)
	}
	if !snap.Stale {
		t.Fatal("an index whose last refresh failed must be marked stale")
	}
}

// The snapshot is handed to request handlers; a caller mutating the slice must
// not be able to reach into the cache.
func TestSnapshotDoesNotShareStorageWithTheCache(t *testing.T) {
	src := &fakeSource{builds: []Build{
		{Version: "v1.4.0", Platforms: []string{"linux-amd64"}},
		{Version: "v1.3.0", Platforms: []string{"linux-amd64"}},
	}}
	c := NewCache(src, at(100))
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	snap := c.Snapshot()
	snap.Builds[0].Version = "tampered"
	snap.Builds[0].Platforms[0] = "tampered"

	again := c.Snapshot()
	if again.Builds[0].Version != "v1.4.0" || again.Builds[0].Platforms[0] != "linux-amd64" {
		t.Fatalf("cache state was reachable through a handed-out snapshot: %+v", again.Builds[0])
	}
}

// A seed list keeps a mother with no route to github.com usable: the operator
// states what exists and the panel can still target it.
func TestSeedBuildsAreOfferedBeforeAnyFetch(t *testing.T) {
	c := NewCache(&fakeSource{}, at(100))
	c.Seed([]Build{{Version: "v1.2.0", Platforms: []string{"linux-amd64"}}})

	snap := c.Snapshot()
	if len(snap.Builds) != 1 || snap.Builds[0].Version != "v1.2.0" {
		t.Fatalf("seed not offered: %+v", snap.Builds)
	}
}

// Both lists are replaced together: a refresh that updated one and kept the
// other would let the panel offer a mother version from a release list that no
// longer describes what is published.
func TestSnapshotCarriesBothFamilies(t *testing.T) {
	c := NewCache(&fakeSource{
		builds: []Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64"}}},
		mother: []Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64", "linux-arm64"}}},
	}, at(100))

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

// The guarantee Builds already had, now for Mother too: handing out the
// cache's own slices would let a handler's incidental write reach shared state.
func TestSnapshotDoesNotShareMotherStorageWithTheCache(t *testing.T) {
	c := NewCache(&fakeSource{
		mother: []Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64"}}},
	}, at(100))
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	first := c.Snapshot()
	first.Mother[0].Platforms[0] = "tampered"
	if c.Snapshot().Mother[0].Platforms[0] != "linux-amd64" {
		t.Fatal("Snapshot handed out the cache's own slice")
	}
}

// A mother that cannot reach the release host cannot download its own
// replacement either, so seeding it a target would only produce a bounded run
// of failed attempts.
func TestSeedOffersNoMotherBuilds(t *testing.T) {
	c := NewCache(&fakeSource{}, at(100))
	c.Seed([]Build{{Version: "v1.4.0", Platforms: []string{"linux-amd64"}}})

	snap := c.Snapshot()
	if len(snap.Builds) != 1 {
		t.Fatalf("the seeded agent builds must be offered: %+v", snap)
	}
	if len(snap.Mother) != 0 {
		t.Fatalf("a seed must not offer the mother a target it cannot fetch: %+v", snap.Mother)
	}
}
