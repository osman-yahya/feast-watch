package store

import (
	"errors"
	"strings"
	"testing"
)

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
