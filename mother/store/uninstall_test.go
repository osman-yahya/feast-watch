package store

import "testing"

// Deleting a server from the panel has to reach the host it is installed on,
// and the only channel to a host is the answer to its own push. So a delete
// becomes a REQUEST recorded on the row, and the row survives until the agent
// reports it is gone.
func TestRequestUninstallMarksTheRow(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("web-1")

	if err := s.RequestUninstall(srv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ServerByID(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UninstallRequestedAt == 0 {
		t.Fatal("uninstall_requested_at not recorded")
	}
}

func TestRequestUninstallUnknownServer(t *testing.T) {
	s := open(t)
	if err := s.RequestUninstall(4242); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Re-requesting must not restart the clock: the panel shows how long a host
// has been stuck removing itself, and an operator pressing Sil twice would
// otherwise reset that to "just now" every time.
func TestRequestUninstallKeepsTheOriginalTimestamp(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("web-1")
	if err := s.RequestUninstall(srv.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := s.ServerByID(srv.ID)

	if _, err := s.DB().Exec(
		`UPDATE servers SET uninstall_requested_at = 1000 WHERE id = ?`, srv.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestUninstall(srv.ID); err != nil {
		t.Fatal(err)
	}
	again, _ := s.ServerByID(srv.ID)
	if again.UninstallRequestedAt != 1000 {
		t.Fatalf("second request moved the timestamp: %d (first was %d)",
			again.UninstallRequestedAt, first.UninstallRequestedAt)
	}
}

// The agent reports why it could not remove itself — no uninstaller on disk, a
// container with no systemd — on every push, exactly like update_error, so a
// retry that succeeds clears it without operator action.
func TestTouchServerWritesUninstallError(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("web-1")
	s.RequestUninstall(srv.ID)

	if err := s.TouchServer(srv.ID, Heartbeat{UninstallError: "uninstaller not found"}, 1700000000); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ServerByID(srv.ID)
	if got.UninstallError != "uninstaller not found" {
		t.Fatalf("uninstall_error = %q", got.UninstallError)
	}

	if err := s.TouchServer(srv.ID, Heartbeat{}, 1700000010); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ServerByID(srv.ID)
	if got.UninstallError != "" {
		t.Fatalf("a clean push must clear the error, got %q", got.UninstallError)
	}
}
