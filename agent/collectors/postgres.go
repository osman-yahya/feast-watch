package collectors

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// A monitoring probe must cost the monitored database as little as possible, so
// the handle is opened once and kept, and it is deliberately capped at a single
// connection: this collector never runs two queries at the same time, and a
// pool would only mean more idle backends on a database that is being watched
// precisely because its connection budget matters.
//
// What makes holding a handle safe is not the caps below but a check the pool
// runs on the way OUT of it. database/sql calls driver.SessionResetter on a
// pooled connection before handing it back to a caller (driverConn.resetSession,
// database/sql/sql.go), and pgx's stdlib *Conn implements it: it rejects a
// closed conn, rejects one left mid-transaction, and — whenever the conn has
// been idle for more than a second, which at every push interval it has — pings
// the server (stdlib/sql.go, Conn.ResetSession). Any of those answers
// driver.ErrBadConn, so the pool closes that conn and retries the query on a
// fresh dial. A Supabase restart, a failover or a NAT idle timeout is caught
// before the query goes out, which matters because a query that dies mid-flight
// is NOT recoverable: pgconn.SafeToRetry is false once the bytes are sent, so
// pgx returns the raw network error and database/sql does not retry it. The
// price is one round-trip per sample. pgx does not implement driver.Validator,
// so nothing is checked on the way back INTO the pool — the check is on the way
// out. Both paths are pinned by
// TestPostgresRecoversWhenTheHeldConnectionDiedWhileIdle and
// TestPostgresReconnectsAfterDatabaseDropsTheConnection.
//
// The caps are age bounds layered on top of that, not the recovery mechanism.
// With a single connection only one of them can be live at a time, and which
// one depends on the push interval the mother hands down (2s-3600s, see
// mother/api/settings.go):
//
//   - Interval shorter than pgConnMaxIdleTime — the default 10s, and every
//     realistic setting: the connection is never idle long enough to be reaped,
//     so pgConnMaxLifetime is the cap that fires. It retires and re-dials the
//     one connection every 30m, so the agent cannot pin a single TCP connection
//     to one pooler backend for the life of the process.
//   - Interval longer than pgConnMaxIdleTime: the connection is reaped between
//     samples and so never reaches 30m of age — pgConnMaxLifetime never fires,
//     and the idle cap is what stops an almost-unused connection occupying a
//     backend slot for the whole gap.
//
// Keeping idle < lifetime is what leaves each cap a reachable regime; invert
// them and the longer one becomes unreachable at every interval.
const (
	pgMaxConns        = 1
	pgConnMaxIdleTime = 5 * time.Minute
	pgConnMaxLifetime = 30 * time.Minute
)

// Postgres reports active connections vs max_connections. Supabase is hosted
// externally, so this collector runs on the backend server's agent and
// queries remotely — deliberately minimal catalog queries (spec).
type Postgres struct {
	dsn   string
	query func(ctx context.Context) (conns, connsMax float64, err error)
	open  func(dsn string) (*sql.DB, error)

	// mu guards the lazily-created handle. *sql.DB is itself concurrency-safe;
	// what needs guarding is the create-once, so two concurrent Collects cannot
	// each build a pool and leak one.
	mu sync.Mutex
	db *sql.DB
}

func NewPostgres(dsn string) *Postgres {
	p := &Postgres{dsn: dsn}
	p.query = p.liveQuery
	p.open = func(dsn string) (*sql.DB, error) { return sql.Open("pgx", dsn) }
	return p
}

func (p *Postgres) Name() string { return "postgres" }

// handle returns the long-lived *sql.DB, creating it on first use. sql.Open
// does not dial, so this costs the database nothing until the first query.
func (p *Postgres) handle() (*sql.DB, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db != nil {
		return p.db, nil
	}
	db, err := p.open(p.dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(pgMaxConns)
	db.SetMaxIdleConns(pgMaxConns)
	db.SetConnMaxIdleTime(pgConnMaxIdleTime)
	db.SetConnMaxLifetime(pgConnMaxLifetime)
	p.db = db
	return db, nil
}

func (p *Postgres) liveQuery(ctx context.Context) (float64, float64, error) {
	db, err := p.handle()
	if err != nil {
		return 0, 0, err
	}

	var conns float64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity`).Scan(&conns); err != nil {
		return 0, 0, err
	}
	var maxStr string
	if err := db.QueryRowContext(ctx, `SHOW max_connections`).Scan(&maxStr); err != nil {
		return 0, 0, err
	}
	maxConns, err := strconv.ParseFloat(maxStr, 64)
	if err != nil {
		return 0, 0, err
	}
	return conns, maxConns, nil
}

func (p *Postgres) Collect(ctx context.Context) ([]Sample, error) {
	conns, maxConns, err := p.query(ctx)
	if err != nil {
		return nil, err
	}
	return []Sample{
		{Key: "postgres.conns", Value: conns},
		{Key: "postgres.conns_max", Value: maxConns},
	}, nil
}
