package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestSetDesiredVersionIsPerServer(t *testing.T) {
	s := open(t)
	canary, _ := s.AddServer("canary")
	other, _ := s.AddServer("other")

	if err := s.SetDesiredVersion(canary.ID, "v1.3.0"); err != nil {
		t.Fatal(err)
	}

	got, _ := s.ServerByID(canary.ID)
	if got.DesiredVersion != "v1.3.0" {
		t.Fatalf("target = %q", got.DesiredVersion)
	}
	untouched, _ := s.ServerByID(other.ID)
	if untouched.DesiredVersion != "" {
		t.Fatalf("rollout leaked to another server: %q", untouched.DesiredVersion)
	}
}

func TestSetDesiredVersionUnknownServer(t *testing.T) {
	s := open(t)
	if err := s.SetDesiredVersion(404, "v1.3.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// Retrying from the panel must start from a clean slate, or the row keeps
// showing the previous attempt's failure while the new one is in flight.
func TestSetDesiredVersionClearsPreviousError(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("web-1")
	s.TouchServer(srv.ID, Heartbeat{AgentVersion: "v1.2.0", UpdateError: "checksum mismatch"}, 100)

	if err := s.SetDesiredVersion(srv.ID, "v1.3.0"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ServerByID(srv.ID)
	if got.UpdateError != "" {
		t.Fatalf("stale error survived a retry: %q", got.UpdateError)
	}
}

// UpdateError rides every push, unlike the identity fields, so an agent that
// recovers clears it without the operator touching anything.
func TestTouchServerClearsUpdateErrorOnRecovery(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("web-1")

	s.TouchServer(srv.ID, Heartbeat{AgentVersion: "v1.2.0", UpdateError: "404 not staged"}, 100)
	if got, _ := s.ServerByID(srv.ID); got.UpdateError != "404 not staged" {
		t.Fatalf("error not stored: %q", got.UpdateError)
	}

	s.TouchServer(srv.ID, Heartbeat{AgentVersion: "v1.3.0"}, 200)
	if got, _ := s.ServerByID(srv.ID); got.UpdateError != "" {
		t.Fatalf("error must clear on a clean push: %q", got.UpdateError)
	}
}

// Identity fields ride the first push only, so a later push carrying none of
// them must not blank out what we already learned.
func TestTouchServerKeepsIdentityWhenNotReported(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("web-1")
	s.TouchServer(srv.ID, Heartbeat{AgentVersion: "v1.2.0", OS: "linux", Arch: "arm64"}, 100)
	s.TouchServer(srv.ID, Heartbeat{AgentVersion: "v1.2.0"}, 200)

	got, _ := s.ServerByID(srv.ID)
	if got.OS != "linux" || got.Arch != "arm64" {
		t.Fatalf("identity erased by a steady-state push: os=%q arch=%q", got.OS, got.Arch)
	}
}

// The fleet-wide setting was retired in favour of per-server targets. Dropping
// it without carrying the value over would silently cancel a rollout an
// operator had already started.
func TestMigrationMovesGlobalDesiredVersionOntoServers(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("web-1")
	if _, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES ('desired_version', 'v1.3.0')`); err != nil {
		t.Fatal(err)
	}
	// Rewind to just before this migration, which is the state a database
	// written by the release that still had the fleet-wide setting is in.
	if _, err := s.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}

	if _, err := migrate(s.db); err != nil {
		t.Fatal(err)
	}

	got, _ := s.ServerByID(srv.ID)
	if got.DesiredVersion != "v1.3.0" {
		t.Fatalf("pending rollout lost: %q", got.DesiredVersion)
	}
	// The key is removed so a later per-server change is not overwritten by a
	// second run of the migration.
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = 'desired_version'`).Scan(&v)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retired setting still present: %q (%v)", v, err)
	}
}

// Migrations are gated on PRAGMA user_version, so a second start applies
// nothing. Re-running them anyway must still be harmless: a rewound version is
// how a restored backup or a rolled-back binary presents itself.
func TestMigrationIsIdempotent(t *testing.T) {
	s := open(t)
	srv, _ := s.AddServer("web-1")
	s.SetDesiredVersion(srv.ID, "v2.0.0")

	if _, err := migrate(s.db); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate(s.db); err != nil {
		t.Fatalf("re-running every migration must be safe: %v", err)
	}

	got, _ := s.ServerByID(srv.ID)
	if got.DesiredVersion != "v2.0.0" {
		t.Fatalf("re-running the migration changed a per-server target: %q", got.DesiredVersion)
	}
}
