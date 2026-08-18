package store

import "testing"

// push applies one agent push, the way ingest does.
func push(t *testing.T, s *Store, serverID, ts int64, samples map[string]float64) {
	t.Helper()
	if err := s.ApplySamples(serverID, ts, samples); err != nil {
		t.Fatal(err)
	}
}

func TestApplySamplesAggregatesIntoTheMinuteBucket(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("a")
	base := int64(1700000000) - 1700000000%60 // minute-aligned

	for i, v := range []float64{10, 20, 30, 40, 50, 60} {
		push(t, s, srv.ID, base+int64(i*10), map[string]float64{"cpu.usage": v})
	}

	var min, max, sum float64
	var cnt int
	row := s.db.QueryRow(
		`SELECT min, max, sum, cnt FROM rollup_1m WHERE server_id=? AND metric='cpu.usage'`, srv.ID)
	if err := row.Scan(&min, &max, &sum, &cnt); err != nil {
		t.Fatal(err)
	}
	if min != 10 || max != 60 || sum != 210 || cnt != 6 {
		t.Fatalf("minute bucket: min=%v max=%v sum=%v cnt=%d", min, max, sum, cnt)
	}
}

// Both tiers are written on the same push, so an hour-range chart never
// depends on a background job having run.
func TestApplySamplesAggregatesIntoTheHourBucketToo(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("a")
	base := int64(1700000000) - 1700000000%3600 // hour-aligned

	// Two different minutes inside one hour.
	push(t, s, srv.ID, base+10, map[string]float64{"cpu.usage": 10})
	push(t, s, srv.ID, base+130, map[string]float64{"cpu.usage": 30})

	var rows int
	s.db.QueryRow(`SELECT COUNT(*) FROM rollup_1m WHERE server_id=?`, srv.ID).Scan(&rows)
	if rows != 2 {
		t.Fatalf("want two minute buckets, got %d", rows)
	}

	var min, max, sum float64
	var cnt int
	row := s.db.QueryRow(
		`SELECT min, max, sum, cnt FROM rollup_1h WHERE server_id=? AND metric='cpu.usage'`, srv.ID)
	if err := row.Scan(&min, &max, &sum, &cnt); err != nil {
		t.Fatal(err)
	}
	if min != 10 || max != 30 || sum != 40 || cnt != 2 {
		t.Fatalf("hour bucket: min=%v max=%v sum=%v cnt=%d", min, max, sum, cnt)
	}
}

// Aggregation is per (server, metric). Mixing two servers into one bucket
// would make every chart a fleet average.
func TestApplySamplesNeverMixesServers(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	b, _ := s.AddServer("b")
	base := int64(1700000000) - 1700000000%60

	for i, v := range []float64{10, 20, 30} {
		push(t, s, a.ID, base+int64(i*10), map[string]float64{"cpu.usage": v})
	}
	for i, v := range []float64{90, 90, 90} {
		push(t, s, b.ID, base+int64(i*10), map[string]float64{"cpu.usage": v})
	}

	for _, tc := range []struct {
		id  int64
		sum float64
		max float64
	}{{a.ID, 60, 30}, {b.ID, 270, 90}} {
		var sum, max float64
		s.db.QueryRow(`SELECT sum, max FROM rollup_1m WHERE server_id=? AND metric='cpu.usage'`,
			tc.id).Scan(&sum, &max)
		if sum != tc.sum || max != tc.max {
			t.Fatalf("server %d: sum=%v max=%v want %v %v", tc.id, sum, max, tc.sum, tc.max)
		}
	}
}

// Every metric in one push shares a transaction, so a mid-push failure cannot
// leave one metric counted and another not.
func TestApplySamplesWritesEveryMetric(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("a")
	push(t, s, srv.ID, 1700000000, map[string]float64{
		"cpu.usage": 1, "mem.used_pct": 2, "disk.used_pct": 3,
	})

	var metrics int
	s.db.QueryRow(`SELECT COUNT(*) FROM rollup_1m WHERE server_id=?`, srv.ID).Scan(&metrics)
	if metrics != 3 {
		t.Fatalf("want a bucket per metric, got %d", metrics)
	}
}

func TestApplySamplesIgnoresAnEmptyPush(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("a")
	if err := s.ApplySamples(srv.ID, 1700000000, nil); err != nil {
		t.Fatal(err)
	}
	var rows int
	s.db.QueryRow(`SELECT COUNT(*) FROM rollup_1m`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("an empty push must write nothing, got %d rows", rows)
	}
}

// The raw tier is gone: nothing reads it, and the chart API floors its
// interval at 60s so the 10-second resolution was unreachable anyway.
func TestSamplesTableIsGone(t *testing.T) {
	s := open(t)
	var name string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='samples'`).Scan(&name)
	if err == nil {
		t.Fatal("the samples table must not be created")
	}
}
