package store

import (
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

// legacyDB builds a database in the shape the previous release wrote: rowid
// rollup tables holding `avg`, a raw `samples` table with its index, and
// user_version 0.
func legacyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`
CREATE TABLE servers (
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, token TEXT UNIQUE NOT NULL,
  collectors TEXT NOT NULL, hostname TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '',
  os TEXT NOT NULL DEFAULT '', agent_version TEXT NOT NULL DEFAULT '',
  last_push INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL
);
CREATE TABLE samples (
  server_id INTEGER NOT NULL, metric TEXT NOT NULL, ts INTEGER NOT NULL, value REAL NOT NULL
);
CREATE INDEX idx_samples ON samples(server_id, metric, ts);
CREATE TABLE rollup_1m (
  server_id INTEGER NOT NULL, metric TEXT NOT NULL, window_start INTEGER NOT NULL,
  min REAL NOT NULL, max REAL NOT NULL, avg REAL NOT NULL, cnt INTEGER NOT NULL,
  PRIMARY KEY (server_id, metric, window_start)
);
CREATE TABLE rollup_1h (
  server_id INTEGER NOT NULL, metric TEXT NOT NULL, window_start INTEGER NOT NULL,
  min REAL NOT NULL, max REAL NOT NULL, avg REAL NOT NULL, cnt INTEGER NOT NULL,
  PRIMARY KEY (server_id, metric, window_start)
);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO settings (key, value) VALUES ('retention_raw_hours', '48');
INSERT INTO rollup_1m VALUES (1, 'cpu.usage', 1700000000, 10, 30, 20, 3);
INSERT INTO rollup_1h VALUES (1, 'cpu.usage', 1700000000, 10, 30, 20, 3);
INSERT INTO samples VALUES (1, 'cpu.usage', 1700000001, 10);
`); err != nil {
		t.Fatal(err)
	}
	return db
}

// The stored history is the only copy left once the raw tier is dropped, so
// the rebuild has to carry it across exactly. avg*cnt is the same weighted
// total the chart query used to compute on every read.
func TestMigrationConvertsAvgToSumWithoutLosingHistory(t *testing.T) {
	db := legacyDB(t)
	if _, err := migrate(db); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"rollup_1m", "rollup_1h"} {
		var min, max, sum float64
		var cnt int
		row := db.QueryRow(`SELECT min, max, sum, cnt FROM ` + table)
		if err := row.Scan(&min, &max, &sum, &cnt); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if min != 10 || max != 30 || sum != 60 || cnt != 3 {
			t.Fatalf("%s: min=%v max=%v sum=%v cnt=%d (want sum=avg*cnt=60)", table, min, max, sum, cnt)
		}
	}
}

// A rowid table keeps a second copy of every primary key in an automatic
// index. The rebuild exists to shed it, so the result must actually be
// WITHOUT ROWID rather than merely have the right columns.
func TestMigrationRebuildsRollupsWithoutRowid(t *testing.T) {
	db := legacyDB(t)
	if _, err := migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"rollup_1m", "rollup_1h"} {
		var ddl string
		if err := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		// rowid tables answer this; WITHOUT ROWID tables reject it.
		if _, err := db.Exec(`SELECT rowid FROM ` + table + ` LIMIT 1`); err == nil {
			t.Fatalf("%s still has a rowid:\n%s", table, ddl)
		}
	}
}

func TestMigrationDropsTheRawTierAndItsSetting(t *testing.T) {
	db := legacyDB(t)
	if _, err := migrate(db); err != nil {
		t.Fatal(err)
	}

	var name string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE name IN ('samples','idx_samples')`).Scan(&name); err == nil {
		t.Fatalf("%s survived the migration", name)
	}
	var value string
	if err := db.QueryRow(
		`SELECT value FROM settings WHERE key = 'retention_raw_hours'`).Scan(&value); err == nil {
		t.Fatalf("the raw retention knob survived with value %q and now bounds nothing", value)
	}
}

// Running the binary twice must not rebuild the tables again — a second
// rebuild would be a full copy of the largest tables on every start.
func TestMigrationRunsOncePerDatabase(t *testing.T) {
	db := legacyDB(t)
	if _, err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO rollup_1m VALUES (2, 'mem.used_pct', 1700000060, 1, 2, 3, 2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate(db); err != nil {
		t.Fatal(err)
	}

	var rows int
	db.QueryRow(`SELECT COUNT(*) FROM rollup_1m`).Scan(&rows)
	if rows != 2 {
		t.Fatalf("a second migrate() disturbed the data: %d rows", rows)
	}
	var version int
	db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != len(migrations) {
		t.Fatalf("user_version %d, want %d", version, len(migrations))
	}
}

// A database that already ran every migration up to the one before the last
// still has to receive the last one. This is the failure mode of an ordered
// list indexed by user_version: an entry inserted anywhere but the end
// renumbers its successors, and a database sitting on the old numbering skips
// exactly one migration — silently, because the version then looks current.
//
// Written as "stand a database at len(migrations)-1 and expect the columns the
// newest migration adds" so it keeps testing the newest one as more are added.
func TestNewestMigrationReachesAnAlreadyMigratedDatabase(t *testing.T) {
	db := legacyDB(t)
	if _, err := migrate(db); err != nil {
		t.Fatal(err)
	}
	// Rewind one version and drop what the last migration added, standing the
	// database exactly where the previous release left it.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations)-1)); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"uninstall_requested_at", "uninstall_error"} {
		if _, err := db.Exec(`ALTER TABLE servers DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO servers (name, token, collectors, created_at, uninstall_requested_at, uninstall_error)
		 VALUES ('web-9','tk_9','[]',1,0,'')`); err != nil {
		t.Fatalf("the newest migration did not reach an already-migrated database: %v", err)
	}
}
