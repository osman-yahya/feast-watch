package store

import (
	"errors"
	"testing"
)

func TestCreateGroupAndList(t *testing.T) {
	s := open(t)
	g, err := s.CreateGroup("Veritabanı Sunucuları")
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == 0 || g.Name != "Veritabanı Sunucuları" {
		t.Fatalf("created: %+v", g)
	}

	groups, err := s.ListGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ServerCount != 0 {
		t.Fatalf("list: %+v", groups)
	}
}

// Group names are never interpolated into a shell script — unlike server names
// — so the charset is broad. The bounds are what matter.
func TestCreateGroupValidatesTheName(t *testing.T) {
	s := open(t)
	for _, name := range []string{"", "   ", "bad\nname", "bad\tname", string(make([]rune, 65))} {
		if _, err := s.CreateGroup(name); !errors.Is(err, ErrInvalidGroupName) {
			t.Fatalf("%q must be rejected, got %v", name, err)
		}
	}
	if _, err := s.CreateGroup("  Prod  "); err != nil {
		t.Fatalf("a trimmable name must be accepted: %v", err)
	}
	groups, _ := s.ListGroups()
	if groups[0].Name != "Prod" {
		t.Fatalf("name not trimmed: %q", groups[0].Name)
	}
}

func TestCreateGroupRejectsADuplicateName(t *testing.T) {
	s := open(t)
	if _, err := s.CreateGroup("Prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGroup("Prod"); !errors.Is(err, ErrDuplicateGroupName) {
		t.Fatalf("want ErrDuplicateGroupName, got %v", err)
	}
}

func TestRenameGroup(t *testing.T) {
	s := open(t)
	g, _ := s.CreateGroup("Prod")
	if err := s.RenameGroup(g.ID, "Production"); err != nil {
		t.Fatal(err)
	}
	groups, _ := s.ListGroups()
	if groups[0].Name != "Production" {
		t.Fatalf("rename: %+v", groups)
	}
	if err := s.RenameGroup(9999, "Nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group: %v", err)
	}
}

// A server may be in several groups: the axes an operator wants to slice by
// (environment, role, region) are independent, and forcing one would mean
// re-tagging the fleet the first time a second axis is needed.
func TestSetGroupServersReplacesMembership(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	b, _ := s.AddServer("b")
	c, _ := s.AddServer("c")
	prod, _ := s.CreateGroup("Prod")
	db, _ := s.CreateGroup("DB")

	if err := s.SetGroupServers(prod.ID, []int64{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroupServers(db.ID, []int64{b.ID, c.ID}); err != nil {
		t.Fatal(err)
	}

	members, err := s.ServersInGroup(prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("prod members: %+v", members)
	}

	// Replace, not append.
	if err := s.SetGroupServers(prod.ID, []int64{c.ID}); err != nil {
		t.Fatal(err)
	}
	members, _ = s.ServersInGroup(prod.ID)
	if len(members) != 1 || members[0].Name != "c" {
		t.Fatalf("membership must be replaced, got %+v", members)
	}

	// b is still in DB — replacing one group must not touch another.
	dbMembers, _ := s.ServersInGroup(db.ID)
	if len(dbMembers) != 2 {
		t.Fatalf("unrelated group disturbed: %+v", dbMembers)
	}
}

func TestSetGroupServersAcceptsAnEmptyList(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	g, _ := s.CreateGroup("Prod")
	s.SetGroupServers(g.ID, []int64{a.ID})

	if err := s.SetGroupServers(g.ID, nil); err != nil {
		t.Fatal(err)
	}
	members, _ := s.ServersInGroup(g.ID)
	if len(members) != 0 {
		t.Fatalf("emptying a group must be possible: %+v", members)
	}
}

func TestSetGroupServersRejectsAnUnknownGroup(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	if err := s.SetGroupServers(9999, []int64{a.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListGroupsCountsMembers(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	b, _ := s.AddServer("b")
	g, _ := s.CreateGroup("Prod")
	s.SetGroupServers(g.ID, []int64{a.ID, b.ID})

	groups, _ := s.ListGroups()
	if groups[0].ServerCount != 2 {
		t.Fatalf("server count: %+v", groups)
	}
}

func TestDeleteGroupRemovesItsMembershipsOnly(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	g, _ := s.CreateGroup("Prod")
	s.SetGroupServers(g.ID, []int64{a.ID})

	if err := s.DeleteGroup(g.ID); err != nil {
		t.Fatal(err)
	}
	if groups, _ := s.ListGroups(); len(groups) != 0 {
		t.Fatalf("group survived: %+v", groups)
	}
	// The server itself is untouched.
	if _, err := s.ServerByID(a.ID); err != nil {
		t.Fatalf("deleting a group must not touch its servers: %v", err)
	}
	var orphans int
	s.db.QueryRow(`SELECT COUNT(*) FROM server_group_members`).Scan(&orphans)
	if orphans != 0 {
		t.Fatalf("%d membership rows orphaned", orphans)
	}
}

// SQLite reuses a deleted server's id, and there are no enforced foreign keys
// on this driver — an orphan membership row would silently enrol a brand-new
// server into a dead server's group and sweep it into that group's next
// rollout.
func TestDeleteServerPurgesItsGroupMemberships(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	g, _ := s.CreateGroup("Prod")
	s.SetGroupServers(g.ID, []int64{a.ID})

	if err := s.DeleteServer(a.ID); err != nil {
		t.Fatal(err)
	}
	var orphans int
	s.db.QueryRow(`SELECT COUNT(*) FROM server_group_members`).Scan(&orphans)
	if orphans != 0 {
		t.Fatalf("%d membership rows survived the server", orphans)
	}
	if groups, _ := s.ListGroups(); groups[0].ServerCount != 0 {
		t.Fatalf("deleted server still counted: %+v", groups)
	}
}

// The server list renders each server's groups, so the whole mapping is read
// once rather than per row.
func TestGroupsByServer(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	b, _ := s.AddServer("b")
	prod, _ := s.CreateGroup("Prod")
	db, _ := s.CreateGroup("DB")
	s.SetGroupServers(prod.ID, []int64{a.ID, b.ID})
	s.SetGroupServers(db.ID, []int64{a.ID})

	byServer, err := s.GroupsByServer()
	if err != nil {
		t.Fatal(err)
	}
	if len(byServer[a.ID]) != 2 {
		t.Fatalf("server a groups: %+v", byServer[a.ID])
	}
	if len(byServer[b.ID]) != 1 || byServer[b.ID][0].Name != "Prod" {
		t.Fatalf("server b groups: %+v", byServer[b.ID])
	}
}

func TestListServersInGroupFilters(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	s.AddServer("b")
	g, _ := s.CreateGroup("Prod")
	s.SetGroupServers(g.ID, []int64{a.ID})

	got, err := s.ServersInGroup(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("filtered list: %+v", got)
	}
}
