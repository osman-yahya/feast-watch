package store

// InsertSamples writes one push's samples as raw rows.
func (s *Store) InsertSamples(serverID int64, ts int64, samples map[string]float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO samples (server_id, metric, ts, value) VALUES (?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for metric, value := range samples {
		if _, err := stmt.Exec(serverID, metric, ts, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RollupSince recomputes rollups for windows >= since. REPLACE makes it
// idempotent; grouping is per (server, metric) — never across servers.
func (s *Store) RollupSince(since int64) error {
	if _, err := s.db.Exec(`
		INSERT OR REPLACE INTO rollup_1m (server_id, metric, window_start, min, max, avg, cnt)
		SELECT server_id, metric, (ts/60)*60, MIN(value), MAX(value), AVG(value), COUNT(*)
		FROM samples WHERE ts >= ?
		GROUP BY server_id, metric, ts/60`, since); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO rollup_1h (server_id, metric, window_start, min, max, avg, cnt)
		SELECT server_id, metric, (window_start/3600)*3600,
		       MIN(min), MAX(max), SUM(avg*cnt)/SUM(cnt), SUM(cnt)
		FROM rollup_1m WHERE window_start >= ?
		GROUP BY server_id, metric, window_start/3600`, since)
	return err
}

// EnforceRetention deletes expired rows per tier (raw 48h, 1m 15d, 1h 75d by default).
func (s *Store) EnforceRetention(now int64, cfg Settings) error {
	cutoffs := []struct {
		table string
		col   string
		limit int64
	}{
		{"samples", "ts", now - int64(cfg.RetentionRawHours)*3600},
		{"rollup_1m", "window_start", now - int64(cfg.Retention1mDays)*86400},
		{"rollup_1h", "window_start", now - int64(cfg.Retention1hDays)*86400},
	}
	for _, c := range cutoffs {
		if _, err := s.db.Exec(`DELETE FROM `+c.table+` WHERE `+c.col+` < ?`, c.limit); err != nil {
			return err
		}
	}
	return nil
}

// DeleteHistory removes stored metrics for a server (0 = all) in [from, to].
func (s *Store) DeleteHistory(serverID int64, from, to int64) error {
	for _, q := range []struct{ table, col string }{
		{"samples", "ts"}, {"rollup_1m", "window_start"}, {"rollup_1h", "window_start"},
	} {
		query := `DELETE FROM ` + q.table + ` WHERE ` + q.col + ` BETWEEN ? AND ?`
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
