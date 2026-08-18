package store

import (
	"fmt"
	"strings"
)

// windowSizes maps each rollup table to the width of its bucket in seconds.
var windowSizes = []struct {
	table string
	width int64
}{
	{"rollup_1m", 60},
	{"rollup_1h", 3600},
}

// ApplySamples folds one push straight into both rollup tiers.
//
// This replaces storing raw samples and reducing them on a timer. The old path
// re-read and rewrote whole windows every 30 seconds — at 50 servers, roughly
// 10,000 rollup rows replaced per tick of which ~95% were byte-identical — and
// it did so through full index scans that held the single write connection.
// Folding on arrival makes each push a bounded write: two upserts per metric,
// touching exactly the buckets that changed.
//
// The whole push shares one transaction, so a failure part-way cannot leave one
// metric counted into a bucket and another not.
func (s *Store) ApplySamples(serverID int64, ts int64, samples map[string]float64) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, w := range windowSizes {
		windowStart := (ts / w.width) * w.width
		// One multi-row statement per tier rather than a prepared loop: the
		// round trips dominate at this size, and every row shares a bucket.
		values := make([]string, 0, len(samples))
		args := make([]any, 0, len(samples)*5)
		for metric, value := range samples {
			values = append(values, "(?,?,?,?,?,?,1)")
			args = append(args, serverID, metric, windowStart, value, value, value)
		}
		// min/max are qualified with the table name so they read as columns;
		// bare `min(min, excluded.min)` is the scalar function applied to the
		// column, which is what we want but is far easier to misread.
		query := fmt.Sprintf(`
			INSERT INTO %[1]s (server_id, metric, window_start, min, max, sum, cnt)
			VALUES %[2]s
			ON CONFLICT(server_id, metric, window_start) DO UPDATE SET
			  min = min(%[1]s.min, excluded.min),
			  max = max(%[1]s.max, excluded.max),
			  sum = %[1]s.sum + excluded.sum,
			  cnt = %[1]s.cnt + 1`, w.table, strings.Join(values, ","))
		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf("apply samples to %s: %w", w.table, err)
		}
	}
	return tx.Commit()
}
