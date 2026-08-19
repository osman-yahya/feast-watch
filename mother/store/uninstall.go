package store

// Two-phase uninstall.
//
// "Delete" in the panel means two different things that a single DELETE cannot
// express: forget this server, and remove the agent from the machine it runs
// on. The second one has no direct channel — the mother never dials an agent
// (see QUICKSTART.md, Network posture) — so it can only travel back in the
// answer to a push the agent itself makes.
//
// So a delete is recorded as a REQUEST and the row stays. Every push from that
// point on is answered with "remove yourself"; the row is dropped only when
// the removal reports success (POST /v1/uninstalled), or when an operator
// forces it for a host that will never push again.

// RequestUninstall marks a server for removal, leaving the row in place.
//
// Idempotent, and deliberately does NOT move an existing timestamp: the panel
// shows how long a host has been stuck removing itself, and an operator
// pressing the button again would otherwise reset that to "just now" — hiding
// exactly the condition they were investigating.
func (s *Store) RequestUninstall(id int64) error {
	res, err := s.db.Exec(`UPDATE servers
		SET uninstall_requested_at = CASE WHEN uninstall_requested_at = 0
		                                  THEN ? ELSE uninstall_requested_at END
		WHERE id = ?`, nowUnix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
