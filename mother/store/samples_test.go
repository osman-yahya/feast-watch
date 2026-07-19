package store

import "testing"

func seed(t *testing.T, s *Store, serverID int64, metric string, base int64, vals []float64) {
	t.Helper()
	for i, v := range vals {
		if err := s.InsertSamples(serverID, base+int64(i*10), map[string]float64{metric: v}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRollupIsPerServerNotAverage(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	b, _ := s.AddServer("b")
	base := int64(1700000000) - 1700000000%60 // minute-aligned

	seed(t, s, a.ID, "cpu.usage", base, []float64{10, 20, 30}) // avg 20
	seed(t, s, b.ID, "cpu.usage", base, []float64{90, 90, 90}) // avg 90

	if err := s.RollupSince(base); err != nil {
		t.Fatal(err)
	}

	var min, max, avg float64
	var cnt int
	row := s.db.QueryRow(`SELECT min, max, avg, cnt FROM rollup_1m WHERE server_id=? AND metric='cpu.usage'`, a.ID)
	if err := row.Scan(&min, &max, &avg, &cnt); err != nil {
		t.Fatal(err)
	}
	if min != 10 || max != 30 || avg != 20 || cnt != 3 {
		t.Fatalf("server a rollup: min=%v max=%v avg=%v cnt=%d", min, max, avg, cnt)
	}
	row = s.db.QueryRow(`SELECT avg FROM rollup_1m WHERE server_id=? AND metric='cpu.usage'`, b.ID)
	row.Scan(&avg)
	if avg != 90 {
		t.Fatalf("server b must keep its own rollup, got avg=%v", avg)
	}
}

func TestRollup1hFrom1m(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	base := int64(1700000000) - 1700000000%3600 // hour-aligned
	seed(t, s, a.ID, "cpu.usage", base, []float64{10, 20})       // minute 1
	seed(t, s, a.ID, "cpu.usage", base+60, []float64{40, 50})    // minute 2
	if err := s.RollupSince(base); err != nil {
		t.Fatal(err)
	}
	var avg float64
	var cnt int
	err := s.db.QueryRow(`SELECT avg, cnt FROM rollup_1h WHERE server_id=?`, a.ID).Scan(&avg, &cnt)
	if err != nil {
		t.Fatal(err)
	}
	if cnt != 4 || avg != 30 { // count-weighted: (10+20+40+50)/4
		t.Fatalf("1h rollup: avg=%v cnt=%d", avg, cnt)
	}
}

func TestRetentionDeletesOldTiers(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	now := int64(1700000000)
	old := now - 49*3600 // older than 48h raw retention
	s.InsertSamples(a.ID, old, map[string]float64{"cpu.usage": 5})
	s.InsertSamples(a.ID, now-10, map[string]float64{"cpu.usage": 6})

	cfg, _ := s.GetSettings()
	if err := s.EnforceRetention(now, cfg); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&n)
	if n != 1 {
		t.Fatalf("raw retention: %d rows left", n)
	}
}

func TestDeleteHistoryByServerAndRange(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	b, _ := s.AddServer("b")
	base := int64(1700000000) - 1700000000%60
	seed(t, s, a.ID, "cpu.usage", base, []float64{1})
	seed(t, s, b.ID, "cpu.usage", base, []float64{2})
	s.RollupSince(base)

	if err := s.DeleteHistory(a.ID, base-100, base+100); err != nil {
		t.Fatal(err)
	}
	var raw, r1m int
	s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE server_id=?`, a.ID).Scan(&raw)
	s.db.QueryRow(`SELECT COUNT(*) FROM rollup_1m WHERE server_id=?`, a.ID).Scan(&r1m)
	if raw != 0 || r1m != 0 {
		t.Fatalf("server a history must be gone: raw=%d r1m=%d", raw, r1m)
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE server_id=?`, b.ID).Scan(&raw)
	if raw != 1 {
		t.Fatal("server b history must survive")
	}
}
