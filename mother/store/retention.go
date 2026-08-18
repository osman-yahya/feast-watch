package store

// EnforceRetention deletes expired rows per tier (1m 15d, 1h 75d by default).
// There is no raw tier left to expire — see schema.go.
func (s *Store) EnforceRetention(now int64, cfg Settings) error {
	cutoffs := []struct {
		table string
		limit int64
	}{
		{"rollup_1m", now - int64(cfg.Retention1mDays)*86400},
		{"rollup_1h", now - int64(cfg.Retention1hDays)*86400},
	}
	for _, c := range cutoffs {
		if _, err := s.db.Exec(
			`DELETE FROM `+c.table+` WHERE window_start < ?`, c.limit); err != nil {
			return err
		}
	}
	return nil
}

// DeleteHistory removes stored metrics for a server (0 = all) in [from, to].
func (s *Store) DeleteHistory(serverID int64, from, to int64) error {
	for _, table := range []string{"rollup_1m", "rollup_1h"} {
		query := `DELETE FROM ` + table + ` WHERE window_start BETWEEN ? AND ?`
		args := []any{from, to}
		if serverID != 0 {
			query += ` AND server_id = ?`
			args = append(args, serverID)
		}
		if _, err := s.db.Exec(query, args...); err != nil {
			return err
		}
	}
	return nil
}
