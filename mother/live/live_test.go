package live

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// clock is a hand-driven time source: the window is expressed in wall time, so
// every eviction assertion here would otherwise need a real sleep.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestStore(window time.Duration) (*Store, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	return New(window, c.now), c
}

func TestSeriesReturnsWhatWasAdded(t *testing.T) {
	s, c := newTestStore(5 * time.Minute)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 12, "memory.usage": 40})
	c.add(10 * time.Second)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 15})

	got := s.Series(1, "cpu.usage")
	want := []Point{{TS: 1_700_000_000, Value: 12}, {TS: 1_700_000_010, Value: 15}}
	if len(got) != len(want) {
		t.Fatalf("got %d points, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("point %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if mem := s.Series(1, "memory.usage"); len(mem) != 1 {
		t.Fatalf("memory series = %+v, want one point", mem)
	}
}

// The returned slice must not alias the stored one: a caller that sorts or
// truncates it would otherwise corrupt the buffer for every later reader.
func TestSeriesReturnsACopy(t *testing.T) {
	s, c := newTestStore(time.Minute)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 12})

	got := s.Series(1, "cpu.usage")
	got[0].Value = 999
	if again := s.Series(1, "cpu.usage"); again[0].Value != 12 {
		t.Fatalf("stored point was mutated through the returned slice: %+v", again)
	}
}

func TestUnknownServerAndMetricAreEmptyNotNilPanics(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	if got := s.Series(42, "cpu.usage"); len(got) != 0 {
		t.Fatalf("unknown server returned %+v", got)
	}
	s.Add(1, time.Now().Unix(), map[string]float64{"cpu.usage": 1})
	if got := s.Series(1, "nope"); len(got) != 0 {
		t.Fatalf("unknown metric returned %+v", got)
	}
}

// Points older than the window are dropped. This is the whole point of the
// store: it holds the last X minutes, not a growing log.
func TestPointsOlderThanTheWindowAreEvicted(t *testing.T) {
	s, c := newTestStore(5 * time.Minute)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 1})
	c.add(4 * time.Minute)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 2})
	c.add(2 * time.Minute) // the first point is now 6 minutes old
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 3})

	got := s.Series(1, "cpu.usage")
	if len(got) != 2 || got[0].Value != 2 || got[1].Value != 3 {
		t.Fatalf("expected the 6-minute-old point to be gone, got %+v", got)
	}
}

// A reader that arrives after the pushes stopped must not be handed stale
// points: eviction cannot depend on a later Add, or a dead agent's last
// minute would linger forever.
func TestReadEvictsWithoutAnyWrite(t *testing.T) {
	s, c := newTestStore(time.Minute)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 1})
	c.add(90 * time.Second)

	if got := s.Series(1, "cpu.usage"); len(got) != 0 {
		t.Fatalf("stale point survived a read-only eviction: %+v", got)
	}
	if vals, ts := s.Latest(1); len(vals) != 0 || ts != 0 {
		t.Fatalf("Latest returned stale data: %+v @ %d", vals, ts)
	}
}

func TestLatestIsTheNewestValuePerMetric(t *testing.T) {
	s, c := newTestStore(5 * time.Minute)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 10, "memory.usage": 50})
	c.add(10 * time.Second)
	// The second push omits memory.usage — a collector that was switched off
	// mid-window. Its last known value stays until it ages out.
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 20})

	vals, ts := s.Latest(1)
	if vals["cpu.usage"] != 20 || vals["memory.usage"] != 50 {
		t.Fatalf("Latest = %+v, want cpu 20 and memory 50", vals)
	}
	if ts != 1_700_000_010 {
		t.Fatalf("Latest ts = %d, want the newest push", ts)
	}
}

func TestForgetDropsTheServer(t *testing.T) {
	s, c := newTestStore(time.Minute)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 1})
	s.Forget(1)
	if got := s.Series(1, "cpu.usage"); len(got) != 0 {
		t.Fatalf("forgotten server still has points: %+v", got)
	}
}

// Shrinking the window has to take effect on the data already held, not only
// on what arrives next: an operator lowering it is asking for the memory back.
func TestSetWindowTrimsWhatIsAlreadyStored(t *testing.T) {
	s, c := newTestStore(10 * time.Minute)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 1})
	c.add(5 * time.Minute)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 2})

	s.SetWindow(time.Minute)
	got := s.Series(1, "cpu.usage")
	if len(got) != 1 || got[0].Value != 2 {
		t.Fatalf("shrinking the window did not trim: %+v", got)
	}
	if s.Window() != time.Minute {
		t.Fatalf("Window() = %s", s.Window())
	}
}

// A window of zero or less would make every point instantly stale; the store
// keeps its previous window rather than becoming a silent black hole.
func TestSetWindowIgnoresNonPositive(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	s.SetWindow(0)
	s.SetWindow(-time.Hour)
	if s.Window() != time.Minute {
		t.Fatalf("Window() = %s, want the original", s.Window())
	}
}

// Metric names come from agents. A compromised or buggy one that rotates the
// name on every push must not be able to grow this map without bound.
func TestDistinctMetricsPerServerAreCapped(t *testing.T) {
	s, c := newTestStore(time.Hour)
	for i := 0; i < MaxMetricsPerServer+50; i++ {
		s.Add(1, c.t.Unix(), map[string]float64{fmt.Sprintf("m%d", i): float64(i)})
	}
	if n := s.MetricCount(1); n != MaxMetricsPerServer {
		t.Fatalf("metric count = %d, want the cap %d", n, MaxMetricsPerServer)
	}
}

// A metric already held keeps being updated after the cap is reached —
// otherwise the first burst of junk names would freeze out cpu.usage.
func TestKnownMetricsStillUpdateAtTheCap(t *testing.T) {
	s, c := newTestStore(time.Hour)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 1})
	for i := 0; i < MaxMetricsPerServer+50; i++ {
		s.Add(1, c.t.Unix(), map[string]float64{fmt.Sprintf("m%d", i): float64(i)})
	}
	c.add(time.Second)
	s.Add(1, c.t.Unix(), map[string]float64{"cpu.usage": 2})

	if vals, _ := s.Latest(1); vals["cpu.usage"] != 2 {
		t.Fatalf("a known metric stopped updating at the cap: %+v", vals["cpu.usage"])
	}
}

// The store is written by every ingest and read by every panel poll, so the
// race detector has to see them overlap.
func TestConcurrentUseIsSafe(t *testing.T) {
	s, c := newTestStore(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Add(int64(n%3), c.t.Unix(), map[string]float64{"cpu.usage": float64(j)})
			}
		}(i)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Series(int64(n%3), "cpu.usage")
				s.Latest(int64(n % 3))
				s.MetricCount(int64(n % 3))
			}
		}(i)
	}
	wg.Wait()
}
