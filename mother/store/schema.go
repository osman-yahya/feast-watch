package store

// schema is the shape a fresh database is created with. Existing databases are
// brought to it by migrate.go — this const is never the whole story on its own,
// because CREATE TABLE IF NOT EXISTS leaves an existing table untouched.
//
// There is deliberately no raw `samples` table. Nothing ever read it except the
// job that produced the rollups from it, and the chart API floors its interval
// at 60 seconds, so the 10-second resolution it stored was unreachable through
// any endpoint. Aggregating on ingest instead of storing and re-reducing turns
// ~32M row-writes a day at 50 servers into ~4M, and drops the largest table.
//
// Both rollup tables are WITHOUT ROWID. Their primary key is the whole access
// path, so a rowid table would keep a second copy of every key in an automatic
// index — measured at 394MB against 226MB for the same 5.4M rows.
//
// `sum` is stored rather than `avg` because the aggregate is maintained
// incrementally: a running sum is exact under repeated addition, while
// recomputing an average from an average needs the count anyway and loses
// precision each time. Readers divide by cnt (see mother/api/chart.go).
const schema = `
CREATE TABLE IF NOT EXISTS servers (
  id            INTEGER PRIMARY KEY,
  name          TEXT UNIQUE NOT NULL,
  token         TEXT UNIQUE NOT NULL,
  collectors    TEXT NOT NULL,
  hostname      TEXT NOT NULL DEFAULT '',
  ip            TEXT NOT NULL DEFAULT '',
  os            TEXT NOT NULL DEFAULT '',
  arch          TEXT NOT NULL DEFAULT '',
  agent_version TEXT NOT NULL DEFAULT '',
  last_push     INTEGER NOT NULL DEFAULT 0,
  capabilities  TEXT NOT NULL DEFAULT '[]',
  desired_version TEXT NOT NULL DEFAULT '',
  update_error    TEXT NOT NULL DEFAULT '',
  -- A delete from the panel cannot reach the host directly (the mother never
  -- dials an agent), so it is recorded here and handed to the agent in the
  -- answer to its next push. The row outlives the request until the agent
  -- reports the removal finished; see store/uninstall.go.
  uninstall_requested_at INTEGER NOT NULL DEFAULT 0,
  uninstall_error        TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS rollup_1m (
  server_id    INTEGER NOT NULL,
  metric       TEXT NOT NULL,
  window_start INTEGER NOT NULL,
  min REAL NOT NULL, max REAL NOT NULL, sum REAL NOT NULL, cnt INTEGER NOT NULL,
  PRIMARY KEY (server_id, metric, window_start)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS rollup_1h (
  server_id    INTEGER NOT NULL,
  metric       TEXT NOT NULL,
  window_start INTEGER NOT NULL,
  min REAL NOT NULL, max REAL NOT NULL, sum REAL NOT NULL, cnt INTEGER NOT NULL,
  PRIMARY KEY (server_id, metric, window_start)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS server_groups (
  id         INTEGER PRIMARY KEY,
  name       TEXT UNIQUE NOT NULL,
  created_at INTEGER NOT NULL
);
-- No foreign keys, deliberately. PRAGMA foreign_keys is off and this driver
-- does not enforce a declared constraint, so declaring one would imply a
-- protection that does not exist. Memberships are purged explicitly by
-- DeleteServer and DeleteGroup instead — which matters because servers.id is
-- an INTEGER PRIMARY KEY without AUTOINCREMENT and SQLite reuses it, so an
-- orphan row would enrol a brand-new server into a dead server's group.
CREATE TABLE IF NOT EXISTS server_group_members (
  group_id  INTEGER NOT NULL,
  server_id INTEGER NOT NULL,
  PRIMARY KEY (group_id, server_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_group_members_server ON server_group_members(server_id);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`
