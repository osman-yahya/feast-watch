package store

import (
	"database/sql"
	"fmt"
)

// applyPragmas configures the connection before any schema work.
//
// WAL is read back rather than assumed: SQLite silently keeps the existing
// journal mode when it cannot switch (an in-memory database, or a file another
// process holds), and a silent no-op here would leave every write taking the
// rollback-journal path while the code claims otherwise.
func applyPragmas(db *sql.DB) error {
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("set journal_mode: %w", err)
	}
	// ":memory:" reports "memory" and cannot do better; a file database that
	// reports anything other than wal did not take the setting.
	if mode != "wal" && mode != "memory" {
		return fmt.Errorf("journal_mode is %q, not wal", mode)
	}
	// NORMAL is the standard pairing with WAL: durable across process crashes,
	// and it stops fsyncing on every commit — which matters when every agent
	// push is a commit.
	if _, err := db.Exec(`PRAGMA synchronous = NORMAL`); err != nil {
		return fmt.Errorf("set synchronous: %w", err)
	}
	return nil
}
