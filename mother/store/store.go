// Package store is the mother's SQLite persistence layer.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

// ErrInvalidName is returned by AddServer when the server name does not
// match validName (see servers.go). Names are rendered raw into the
// install shell script, so charset validation is enforced at this boundary.
var ErrInvalidName = errors.New("invalid server name: allowed [A-Za-z0-9._-], max 64 chars")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite: single writer; serialize access instead of returning
	// SQLITE_BUSY. WAL (see applyPragmas) is what keeps that from also
	// serializing readers behind the writer.
	db.SetMaxOpenConns(1)
	for _, step := range []func(*sql.DB) error{applyPragmas, createSchema} {
		if err := step(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	applied, err := migrate(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	// Dropping the raw tier and rebuilding the rollups frees pages inside the
	// file; SQLite does not hand them back without being asked. VACUUM cannot
	// run inside a transaction, so it sits here rather than in the migration,
	// and only after one actually ran — it rewrites the whole database, which
	// is a cost worth paying once at upgrade and never on a routine start.
	if applied > 0 {
		if _, err := db.Exec(`VACUUM`); err != nil {
			db.Close()
			return nil, fmt.Errorf("vacuum after migration: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func createSchema(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read-only query composition in the api package.
func (s *Store) DB() *sql.DB { return s.db }
