package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// migrations are applied in order to any database whose PRAGMA user_version is
// below their index+1, then user_version is advanced.
//
// This replaces an append-only list of ALTERs whose success case was "fails
// with duplicate column name". That pattern cannot express anything but adding
// a column — no table rebuild, no data transform — and it re-ran every
// statement on every start, swallowing a class of real errors to do it.
//
// Each entry runs inside one transaction together with the version bump, so a
// half-applied migration is not a state the mother can start in.
var migrations = []struct {
	name string
	run  func(*sql.Tx) error
}{
	{"servers: columns added after the first release", func(tx *sql.Tx) error {
		// Pre-user_version databases may already have these. addColumn treats
		// "duplicate column name" as success; nothing else does.
		for _, c := range []string{
			`capabilities TEXT NOT NULL DEFAULT '[]'`,
			`arch TEXT NOT NULL DEFAULT ''`,
			`desired_version TEXT NOT NULL DEFAULT ''`,
			`update_error TEXT NOT NULL DEFAULT ''`,
		} {
			if err := addColumn(tx, "servers", c); err != nil {
				return err
			}
		}
		return nil
	}},

	{"settings: retire the fleet-wide desired_version onto each server", migrateDesiredVersionPerServer},

	{"rollups: store sum instead of avg, WITHOUT ROWID, and drop the raw tier", migrateRollupsToSum},

	{"server groups", func(tx *sql.Tx) error {
		// CREATE TABLE IF NOT EXISTS in the schema already made these on a
		// fresh database; this entry exists so an older one gets them too and
		// the version numbering stays honest about what changed.
		_, err := tx.Exec(`
			CREATE TABLE IF NOT EXISTS server_groups (
			  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, created_at INTEGER NOT NULL
			);
			CREATE TABLE IF NOT EXISTS server_group_members (
			  group_id INTEGER NOT NULL, server_id INTEGER NOT NULL,
			  PRIMARY KEY (group_id, server_id)
			) WITHOUT ROWID;
			CREATE INDEX IF NOT EXISTS idx_group_members_server ON server_group_members(server_id);`)
		return err
	}},
}

// migrate brings db up to the current schema version and reports how many
// migrations it had to run, so the caller can decide whether reclaiming space
// is worth it.
func migrate(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	applied := 0
	for i := version; i < len(migrations); i++ {
		m := migrations[i]
		if err := applyMigration(db, i, m.run); err != nil {
			return applied, fmt.Errorf("migration %d (%s): %w", i+1, m.name, err)
		}
		applied++
	}
	return applied, nil
}

func applyMigration(db *sql.DB, index int, run func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := run(tx); err != nil {
		return err
	}
	// PRAGMA does not accept a bound parameter; index is loop-controlled, not
	// caller-supplied, so the interpolation has no external input.
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, index+1)); err != nil {
		return err
	}
	return tx.Commit()
}

// addColumn adds a column, treating "already there" as success — the only
// outcome an ALTER on an already-migrated database can have.
func addColumn(tx *sql.Tx, table, definition string) error {
	_, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + definition)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add %s.%s: %w", table, definition, err)
	}
	return nil
}

// migrateDesiredVersionPerServer moves the retired fleet-wide
// settings.desired_version onto each server row. Dropping the setting without
// this would silently cancel a rollout an operator had already started.
func migrateDesiredVersionPerServer(tx *sql.Tx) error {
	var global string
	err := tx.QueryRow(`SELECT value FROM settings WHERE key = 'desired_version'`).Scan(&global)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if global != "" {
		if _, err := tx.Exec(
			`UPDATE servers SET desired_version = ? WHERE desired_version = ''`, global); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`DELETE FROM settings WHERE key = 'desired_version'`)
	return err
}

// migrateRollupsToSum rebuilds both rollup tiers as WITHOUT ROWID tables
// holding a running `sum` instead of an `avg`, and drops the raw `samples`
// table with its index.
//
// A rebuild rather than an ALTER: WITHOUT ROWID cannot be turned on after the
// fact, and `avg` has to become `avg*cnt` row by row on the way across. Both
// tiers are recomputed from what is already stored, so no history is lost —
// the raw tier is dropped last and is not needed to produce either.
func migrateRollupsToSum(tx *sql.Tx) error {
	for _, table := range []string{"rollup_1m", "rollup_1h"} {
		hasAvg, err := columnExists(tx, table, "avg")
		if err != nil {
			return err
		}
		if !hasAvg {
			continue // already the new shape, or a fresh database
		}
		rebuilt := table + "_rebuilt"
		if _, err := tx.Exec(fmt.Sprintf(`
			CREATE TABLE %s (
			  server_id    INTEGER NOT NULL,
			  metric       TEXT NOT NULL,
			  window_start INTEGER NOT NULL,
			  min REAL NOT NULL, max REAL NOT NULL, sum REAL NOT NULL, cnt INTEGER NOT NULL,
			  PRIMARY KEY (server_id, metric, window_start)
			) WITHOUT ROWID`, rebuilt)); err != nil {
			return err
		}
		// avg*cnt reconstructs the sum the old writer never stored. It is the
		// same weighted total the chart query already computed on read.
		if _, err := tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (server_id, metric, window_start, min, max, sum, cnt)
			SELECT server_id, metric, window_start, min, max, avg*cnt, cnt FROM %s`,
			rebuilt, table)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP TABLE ` + table); err != nil {
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, rebuilt, table)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_samples`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS samples`); err != nil {
		return err
	}
	// The raw tier's retention knob no longer describes anything.
	_, err := tx.Exec(`DELETE FROM settings WHERE key = 'retention_raw_hours'`)
	return err
}

func columnExists(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
