// Package live keeps the most recent samples of every server in memory.
//
// It exists because the mother stores no raw tier: ApplySamples folds each
// push straight into rollup_1m and rollup_1h (see store/rollup.go), and the
// chart API floors its interval at 60 seconds. That is the right shape for
// history — but it means the panel cannot show what a server is doing *now*
// at the resolution the agents actually push at. This store is that missing
// resolution, and only that: the last X minutes, in RAM.
//
// Deliberately in-process memory and nothing else. No Redis, no table, no
// file: the data is worth exactly one mother lifetime. A restart empties it
// and the next pushes refill it within one window, which is why nothing here
// is persisted or replicated — buying durability for a live view would cost a
// write per push, which is precisely the write volume the raw tier was
// dropped to avoid.
//
// Memory is bounded on three axes: the time window (operator-configurable),
// the ingest rate limit (one push per server per 2s, so a window can hold at
// most window/2s points per metric), and MaxMetricsPerServer.
package live

import (
	"sort"
	"sync"
	"time"
)

// MaxMetricsPerServer caps how many distinct metric names one server may
// occupy. Metric names are agent-supplied: ingest validates their charset and
// bounds a single push at 256 samples, but nothing stops an agent from
// rotating the name on every push, which would grow this map without bound
// for as long as the mother runs. At the cap a name that is already held keeps
// updating and a new one is dropped — the fleet's real metric set is stable
// and an order of magnitude smaller, so this can only bind on junk.
const MaxMetricsPerServer = 256

// Point is one sample at one moment.
type Point struct {
	TS    int64   `json:"ts"`
	Value float64 `json:"value"`
}

// Store holds the last Window() of samples for every server.
//
// The zero value is not usable; call New.
type Store struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time

	// series[serverID][metric] is ascending by timestamp. Pushes arrive in
	// mother-clock order (ingest stamps them with its own time.Now), so
	// appending keeps that order without a sort.
	series map[int64]map[string][]Point
}

// New returns a Store holding `window` of samples, reading time from now.
// A non-positive window falls back to a minute rather than making every point
// instantly stale.
func New(window time.Duration, now func() time.Time) *Store {
	if window <= 0 {
		window = time.Minute
	}
	if now == nil {
		now = time.Now
	}
	return &Store{window: window, now: now, series: map[int64]map[string][]Point{}}
}

// Add records one push. ts is the mother's own timestamp for it, in unix
// seconds — the same value written to the rollups, so the live view and the
// history agree about when a sample happened.
func (s *Store) Add(serverID int64, ts int64, samples map[string]float64) {
	if len(samples) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	byMetric := s.series[serverID]
	if byMetric == nil {
		byMetric = map[string][]Point{}
		s.series[serverID] = byMetric
	}
	for metric, value := range samples {
		if _, known := byMetric[metric]; !known && len(byMetric) >= MaxMetricsPerServer {
			continue
		}
		byMetric[metric] = append(byMetric[metric], Point{TS: ts, Value: value})
	}
	s.evictLocked(serverID)
}

// Series returns the points held for one metric, oldest first.
//
// The result is a copy: callers hand it to a JSON encoder or sort it, and
// aliasing the live buffer would let one reader corrupt it for every other.
func (s *Store) Series(serverID int64, metric string) []Point {
	return s.SeriesSince(serverID, metric, 0)
}

// SeriesSince returns the points held for one metric that are strictly newer
// than `since`, oldest first. A since of 0 means "everything", which is what a
// reader holding nothing yet asks for — timestamps are unix seconds, so 0 is
// never a real sample.
//
// This exists for the polling reader. A page showing the window already holds
// every point but the last few, so re-sending the whole buffer on a cadence
// derived from the push interval is the same bytes over and over: an hour of
// 10-second samples across eight metrics is ~60KB per poll, against a few
// hundred bytes for what actually changed. Strictly newer, not newer-or-equal,
// because the reader passes back the newest timestamp it holds and returning
// that point again would duplicate the last sample on every poll.
//
// The result is a copy, for the same reason Series returns one.
func (s *Store) SeriesSince(serverID int64, metric string, since int64) []Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked(serverID)

	src := s.series[serverID][metric]
	// Ascending order makes the answer a suffix, so it is found by search
	// rather than by filtering: a poll that adds one point must not cost a
	// pass over the whole window.
	first := sort.Search(len(src), func(i int) bool { return src[i].TS > since })
	if first == len(src) {
		return nil
	}
	out := make([]Point, len(src)-first)
	copy(out, src[first:])
	return out
}

// Latest returns the newest value of every metric still inside the window,
// together with the timestamp of the newest of them (0 when there is nothing).
//
// Per-metric rather than "the last push": a collector switched off mid-window,
// or one that reports on its own slower cadence, still has a last known value
// and dropping it would blank a chart that is merely quiet.
func (s *Store) Latest(serverID int64) (map[string]float64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked(serverID)

	byMetric := s.series[serverID]
	if len(byMetric) == 0 {
		return nil, 0
	}
	out := make(map[string]float64, len(byMetric))
	var newest int64
	for metric, points := range byMetric {
		last := points[len(points)-1]
		out[metric] = last.Value
		if last.TS > newest {
			newest = last.TS
		}
	}
	return out, newest
}

// MetricCount reports how many distinct metrics are held for a server. It is
// the observable side of MaxMetricsPerServer.
func (s *Store) MetricCount(serverID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked(serverID)
	return len(s.series[serverID])
}

// Forget drops everything held for a server. Called when a server is deleted:
// SQLite reuses INTEGER PRIMARY KEY values, so leftover points would otherwise
// surface as a brand-new server's history.
func (s *Store) Forget(serverID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.series, serverID)
}

// SetWindow changes how much is kept and trims what is already held, so
// lowering it hands the memory back at once instead of at the next push. A
// non-positive value is ignored.
func (s *Store) SetWindow(window time.Duration) {
	if window <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.window = window
	for serverID := range s.series {
		s.evictLocked(serverID)
	}
}

// Window reports the retention window currently in force.
func (s *Store) Window() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.window
}

// evictLocked drops everything older than the window for one server, and any
// metric or server left holding nothing.
//
// It runs on reads as well as writes on purpose: eviction that only happened
// on Add would keep a dead agent's last minute visible forever, which is the
// one thing a *live* view must never show.
func (s *Store) evictLocked(serverID int64) {
	byMetric := s.series[serverID]
	if byMetric == nil {
		return
	}
	cutoff := s.now().Add(-s.window).Unix()
	for metric, points := range byMetric {
		// Ascending order means the survivors are a suffix: find the first
		// point at or after the cutoff and keep from there.
		keep := 0
		for keep < len(points) && points[keep].TS < cutoff {
			keep++
		}
		switch {
		case keep == 0:
			// Nothing expired; leave the slice alone.
		case keep == len(points):
			delete(byMetric, metric)
		default:
			// Compact in place rather than resliceing: a slice that only ever
			// moves its start keeps the whole original array alive.
			byMetric[metric] = append(points[:0], points[keep:]...)
		}
	}
	if len(byMetric) == 0 {
		delete(s.series, serverID)
	}
}
