package store

import (
	"errors"
	"strings"
	"testing"
)

func TestAddServerRejectsInvalidNames(t *testing.T) {
	invalid := []string{
		"evil; curl x | bash #",
		"has space",
		"a$(cmd)",
		strings.Repeat("a", 65),
		"",
	}
	for _, name := range invalid {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			if _, err := s.AddServer(name); !errors.Is(err, ErrInvalidName) {
				t.Fatalf("want ErrInvalidName for %q, got %v", name, err)
			}
		})
	}

	valid := []string{"DB_Sunucusu", "web-1", "a.b"}
	for _, name := range valid {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			if _, err := s.AddServer(name); err != nil {
				t.Fatalf("want success for %q, got %v", name, err)
			}
		})
	}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAddServerGeneratesTokenAndDefaults(t *testing.T) {
	s := open(t)
	srv, err := s.AddServer("DB_Sunucusu")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(srv.Token, "tk_") || len(srv.Token) != 35 {
		t.Fatalf("token format: %q", srv.Token)
	}
	if len(srv.Collectors) != 4 || srv.Collectors[0] != "cpu" {
		t.Fatalf("default collectors: %v", srv.Collectors)
	}
	if _, err := s.AddServer("DB_Sunucusu"); err == nil {
		t.Fatal("duplicate name must fail")
	}
}

func TestServerByTokenAndTouch(t *testing.T) {
	s := open(t)
	created, _ := s.AddServer("web-1")

	got, err := s.ServerByToken(created.Token)
	if err != nil || got.Name != "web-1" {
		t.Fatalf("lookup: %v %+v", err, got)
	}

	if err := s.TouchServer(got.ID, "1.2.0", "web1-host", "10.0.0.7", "linux", 1700000000); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListServers()
	if list[0].AgentVersion != "1.2.0" || list[0].LastPush != 1700000000 || list[0].IP != "10.0.0.7" {
		t.Fatalf("touch not persisted: %+v", list[0])
	}

	if _, err := s.ServerByToken("tk_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteServerRevokesToken(t *testing.T) {
	s := open(t)
	created, _ := s.AddServer("gone")
	if err := s.DeleteServer(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ServerByToken(created.Token); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted server's token must be rejected")
	}
}

// TestDeleteServerPurgesRollupHistory guards against rowid reuse: the
// servers table is an INTEGER PRIMARY KEY without AUTOINCREMENT, so SQLite
// is free to reuse a deleted row's id for the next inserted row. If
// DeleteServer leaves rollup_1m/rollup_1h rows behind, a brand-new server
// that inherits the old id would appear to have historical metrics it never
// actually reported.
func TestDeleteServerPurgesRollupHistory(t *testing.T) {
	s := open(t)
	a, err := s.AddServer("server-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSamples(a.ID, 1700000000, map[string]float64{"cpu.usage": 42}); err != nil {
		t.Fatal(err)
	}
	if err := s.RollupSince(1700000000); err != nil {
		t.Fatal(err)
	}

	countRollups := func(id int64) (m1, h1 int) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM rollup_1m WHERE server_id = ?`, id).Scan(&m1); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM rollup_1h WHERE server_id = ?`, id).Scan(&h1); err != nil {
			t.Fatal(err)
		}
		return
	}

	if m1, h1 := countRollups(a.ID); m1 == 0 || h1 == 0 {
		t.Fatalf("setup: expected rollups for server-a, got rollup_1m=%d rollup_1h=%d", m1, h1)
	}

	if err := s.DeleteServer(a.ID); err != nil {
		t.Fatal(err)
	}

	fresh, err := s.AddServer("fresh")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID != a.ID {
		t.Skipf("rowid was not reused (got %d, want %d) — cannot exercise the leak on this SQLite build", fresh.ID, a.ID)
	}

	if m1, h1 := countRollups(fresh.ID); m1 != 0 || h1 != 0 {
		t.Fatalf("fresh server inherited deleted server's rollup history: rollup_1m=%d rollup_1h=%d", m1, h1)
	}
}
