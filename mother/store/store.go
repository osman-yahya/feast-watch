// Package store is the mother's SQLite persistence layer.
package store

import (
	"database/sql"
	"errors"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS servers (
  id            INTEGER PRIMARY KEY,
  name          TEXT UNIQUE NOT NULL,
  token         TEXT UNIQUE NOT NULL,
  collectors    TEXT NOT NULL,
  hostname      TEXT NOT NULL DEFAULT '',
  ip            TEXT NOT NULL DEFAULT '',
  os            TEXT NOT NULL DEFAULT '',
  agent_version TEXT NOT NULL DEFAULT '',
  last_push     INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS samples (
  server_id INTEGER NOT NULL,
  metric    TEXT NOT NULL,
  ts        INTEGER NOT NULL,
  value     REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples ON samples(server_id, metric, ts);
CREATE TABLE IF NOT EXISTS rollup_1m (
  server_id    INTEGER NOT NULL,
  metric       TEXT NOT NULL,
  window_start INTEGER NOT NULL,
  min REAL NOT NULL, max REAL NOT NULL, avg REAL NOT NULL, cnt INTEGER NOT NULL,
  PRIMARY KEY (server_id, metric, window_start)
);
CREATE TABLE IF NOT EXISTS rollup_1h (
  server_id    INTEGER NOT NULL,
  metric       TEXT NOT NULL,
  window_start INTEGER NOT NULL,
  min REAL NOT NULL, max REAL NOT NULL, avg REAL NOT NULL, cnt INTEGER NOT NULL,
  PRIMARY KEY (server_id, metric, window_start)
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite: single writer; serialize access instead of returning SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read-only query composition in the api package.
func (s *Store) DB() *sql.DB { return s.db }
