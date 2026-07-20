package collectors

import (
	"context"
	"database/sql"
	"strconv"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Postgres reports active connections vs max_connections. Supabase is hosted
// externally, so this collector runs on the backend server's agent and
// queries remotely — deliberately minimal catalog queries (spec).
type Postgres struct {
	dsn   string
	query func(ctx context.Context) (conns, connsMax float64, err error)
}

func NewPostgres(dsn string) *Postgres {
	p := &Postgres{dsn: dsn}
	p.query = p.liveQuery
	return p
}

func (p *Postgres) Name() string { return "postgres" }

func (p *Postgres) liveQuery(ctx context.Context) (float64, float64, error) {
	db, err := sql.Open("pgx", p.dsn)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()

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
