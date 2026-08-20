package store

import "database/sql"

// MotherUpdate is the mother's own rollout intent and how far it has got.
//
// The zero value is "nothing to do", which is what both a fresh database and a
// finished update read as — there is no separate idle marker to keep in step
// with the rest of the row.
type MotherUpdate struct {
	DesiredVersion string
	StagedVersion  string
	Attempts       int
	Error          string
	RequestedAt    int64
	AppliedAt      int64
}

// motherUpdateRow is the singleton's id. Named rather than inlined so the
// CHECK constraint in the schema and every statement here agree by
// construction.
const motherUpdateRow = 1

func (s *Store) MotherUpdate() (MotherUpdate, error) {
	var out MotherUpdate
	err := s.db.QueryRow(
		`SELECT desired_version, staged_version, attempts, error, requested_at, applied_at
		   FROM mother_update WHERE id = ?`, motherUpdateRow).
		Scan(&out.DesiredVersion, &out.StagedVersion, &out.Attempts, &out.Error,
			&out.RequestedAt, &out.AppliedAt)
	if err == sql.ErrNoRows {
		// The row is created by the first write, so its absence is the zero
		// intent rather than a fault.
		return MotherUpdate{}, nil
	}
	return out, err
}

// SetMotherDesiredVersion records a target, resetting everything a previous
// target left behind. An operator's fresh decision must not inherit the
// attempt budget the last one spent, or the error that explained why it
// stopped. An empty version cancels.
func (s *Store) SetMotherDesiredVersion(version string, now int64) error {
	_, err := s.db.Exec(
		`INSERT INTO mother_update (id, desired_version, staged_version, attempts, error, requested_at)
		 VALUES (?, ?, '', 0, '', ?)
		 ON CONFLICT(id) DO UPDATE SET
		   desired_version = excluded.desired_version,
		   staged_version  = '',
		   attempts        = 0,
		   error           = '',
		   requested_at    = excluded.requested_at`,
		motherUpdateRow, version, now)
	return err
}

// BeginMotherAttempt counts one attempt against the target and returns the new
// total.
//
// It is called — and committed — BEFORE the download. A counter written after
// a step that never completes counts nothing, and this counter is the only
// thing standing between a bad target and a download-exit-restart loop that
// never explains itself. The previous error is cleared with it: it described
// the attempt that just ended, not the one starting.
func (s *Store) BeginMotherAttempt() (int, error) {
	if _, err := s.db.Exec(
		`UPDATE mother_update SET attempts = attempts + 1, error = '' WHERE id = ?`,
		motherUpdateRow); err != nil {
		return 0, err
	}
	row, err := s.MotherUpdate()
	return row.Attempts, err
}

// StageMotherUpdate records that a verified binary is waiting for the promote
// helper to install it at the next start.
func (s *Store) StageMotherUpdate(version string) error {
	_, err := s.db.Exec(
		`UPDATE mother_update SET staged_version = ? WHERE id = ?`, version, motherUpdateRow)
	return err
}

// RecordMotherUpdateError leaves the target in place: the failure may well be
// transient, and the attempt counter is what bounds the retrying.
func (s *Store) RecordMotherUpdateError(msg string) error {
	_, err := s.db.Exec(
		`UPDATE mother_update SET error = ? WHERE id = ?`, msg, motherUpdateRow)
	return err
}

// FailMotherUpdate gives up on the target but keeps the reason. Dropping both
// would leave an operator with a mother that quietly stayed where it was and
// nothing at all to explain why.
func (s *Store) FailMotherUpdate(msg string) error {
	_, err := s.db.Exec(
		`UPDATE mother_update SET desired_version = '', staged_version = '', error = ? WHERE id = ?`,
		msg, motherUpdateRow)
	return err
}

// ClearMotherUpdate is the successful end: the running version is the wanted
// one, so nothing about the last update is still true except when it landed.
func (s *Store) ClearMotherUpdate(appliedAt int64) error {
	_, err := s.db.Exec(
		`INSERT INTO mother_update (id, applied_at) VALUES (?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   desired_version = '', staged_version = '', attempts = 0, error = '',
		   applied_at = excluded.applied_at`,
		motherUpdateRow, appliedAt)
	return err
}
