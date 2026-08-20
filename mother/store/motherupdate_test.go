package store

import "testing"

func TestMotherUpdateStartsEmpty(t *testing.T) {
	s := open(t)
	got, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got != (MotherUpdate{}) {
		t.Fatalf("a fresh database must hold no update intent: %+v", got)
	}
}

// A fresh decision by an operator must not inherit the attempt budget a
// previous one spent, nor the error that explained why it stopped.
func TestSetMotherDesiredVersionResetsTheAttemptBudget(t *testing.T) {
	s := open(t)
	if err := s.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginMotherAttempt(); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMotherUpdateError("checksum mismatch"); err != nil {
		t.Fatal(err)
	}
	if err := s.StageMotherUpdate("v1.4.0"); err != nil {
		t.Fatal(err)
	}

	if err := s.SetMotherDesiredVersion("v1.5.0", 200); err != nil {
		t.Fatal(err)
	}
	got, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredVersion != "v1.5.0" || got.Attempts != 0 || got.Error != "" || got.StagedVersion != "" {
		t.Fatalf("a new target must start clean: %+v", got)
	}
	if got.RequestedAt != 200 {
		t.Fatalf("requested_at: %d", got.RequestedAt)
	}
}

// The counter is what bounds a mother that restarts into the same failure, so
// it has to survive the restart — committed, not held in memory.
func TestBeginMotherAttemptCountsUpAndPersists(t *testing.T) {
	s := open(t)
	if err := s.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 3; want++ {
		got, err := s.BeginMotherAttempt()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("attempt %d reported as %d", want, got)
		}
	}
	row, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.Attempts != 3 {
		t.Fatalf("attempts: %d", row.Attempts)
	}
}

// Failing drops the target but keeps the reason: the target is what would
// otherwise be retried forever, the reason is the only thing that explains it.
func TestFailMotherUpdateDropsTheTargetAndKeepsTheReason(t *testing.T) {
	s := open(t)
	if err := s.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.FailMotherUpdate("giving up after 3 attempts"); err != nil {
		t.Fatal(err)
	}
	got, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredVersion != "" {
		t.Fatalf("target must be cleared: %+v", got)
	}
	if got.Error != "giving up after 3 attempts" {
		t.Fatalf("error: %q", got.Error)
	}
}

func TestClearMotherUpdateResetsEverythingAndStampsApplied(t *testing.T) {
	s := open(t)
	if err := s.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginMotherAttempt(); err != nil {
		t.Fatal(err)
	}
	if err := s.StageMotherUpdate("v1.4.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearMotherUpdate(300); err != nil {
		t.Fatal(err)
	}

	got, err := s.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredVersion != "" || got.StagedVersion != "" || got.Attempts != 0 || got.Error != "" {
		t.Fatalf("row must be idle: %+v", got)
	}
	if got.AppliedAt != 300 {
		t.Fatalf("applied_at: %d", got.AppliedAt)
	}
}

// One mother per deployment, and "which version should I be" is a property of
// the process rather than of a collection. The CHECK is what keeps a second
// row from ever existing to disagree with the first.
func TestMotherUpdateHoldsExactlyOneRow(t *testing.T) {
	s := open(t)
	if err := s.SetMotherDesiredVersion("v1.4.0", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO mother_update (id, desired_version) VALUES (2, 'v9.9.9')`); err == nil {
		t.Fatal("a second row was accepted")
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mother_update`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows: %d", rows)
	}
}
