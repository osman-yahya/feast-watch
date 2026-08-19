package collectors

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostgresReportsConnsVsMax(t *testing.T) {
	p := NewPostgres("postgres://ignored")
	p.query = func(ctx context.Context) (conns, connsMax float64, err error) {
		return 42, 100, nil
	}
	got, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["postgres.conns"] != 42 || byKey["postgres.conns_max"] != 100 {
		t.Fatalf("got %v", byKey)
	}
}

// --- fake database/sql driver -------------------------------------------------
//
// The postgres collector must not be tested against a live database, so these
// stand in for one. fakePG counts driver-level dials (what the monitored server
// actually pays for) separately from sql.Open calls (what the collector does),
// which is the whole point: one sql.Open, one dial, reused across samples.

type fakePG struct {
	mu       sync.Mutex
	dials    int // driver.Open calls == real connection setups
	failNext int // this many upcoming queries return driver.ErrBadConn

	// gen models the server's incarnation. restart() bumps it; every conn
	// dialled before that point is now talking to a socket the server end has
	// forgotten, exactly as after a Supabase restart or a NAT idle timeout.
	gen int
}

// errServerGone is what a stale connection's query fails with. Deliberately not
// driver.ErrBadConn: once the query bytes are on the wire pgx cannot know the
// server never saw them (pgconn.SafeToRetry is false), so it surfaces the raw
// network error and database/sql will NOT retry it on a fresh connection. A
// sample lost this way is lost. That is why the liveness check has to happen at
// checkout, before the query.
var errServerGone = errors.New("read tcp: connection reset by peer")

func (f *fakePG) Open(string) (driver.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	return &fakePGConn{pg: f, gen: f.gen}, nil
}

func (f *fakePG) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gen++
}

func (f *fakePG) dialCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials
}

type fakePGConn struct {
	pg  *fakePG
	gen int
}

func (c *fakePGConn) stale() bool {
	c.pg.mu.Lock()
	defer c.pg.mu.Unlock()
	return c.gen != c.pg.gen
}

func (c *fakePGConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (c *fakePGConn) Close() error                        { return nil }
func (c *fakePGConn) Begin() (driver.Tx, error)           { return nil, errors.New("no tx") }

// ResetSession models pgx's own (stdlib/sql.go): database/sql calls it before
// reusing a pooled connection, and pgx answers it by pinging the server when the
// connection has been idle more than a second. A dead connection therefore
// reports driver.ErrBadConn here, which is a retryable answer — database/sql
// closes it and comes back for a fresh dial. This is the collector's whole
// safety story for holding one connection open for the life of the process, so
// the fake driver has to have it or the test below is testing nothing.
func (c *fakePGConn) ResetSession(context.Context) error {
	if c.stale() {
		return driver.ErrBadConn
	}
	return nil
}

func (c *fakePGConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.stale() {
		return nil, errServerGone
	}

	c.pg.mu.Lock()
	if c.pg.failNext > 0 {
		c.pg.failNext--
		c.pg.mu.Unlock()
		return nil, driver.ErrBadConn
	}
	c.pg.mu.Unlock()

	switch {
	case strings.Contains(q, "pg_stat_activity"):
		return &fakePGRows{cols: []string{"count"}, row: []driver.Value{int64(7)}}, nil
	case strings.Contains(q, "max_connections"):
		return &fakePGRows{cols: []string{"max_connections"}, row: []driver.Value{"100"}}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", q)
}

type fakePGRows struct {
	cols []string
	row  []driver.Value
	done bool
}

func (r *fakePGRows) Columns() []string { return r.cols }
func (r *fakePGRows) Close() error      { return nil }
func (r *fakePGRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dest, r.row)
	r.done = true
	return nil
}

var fakePGSeq atomic.Int64

// newFakePGCollector wires a Postgres collector to the fake driver through a
// counting opener, and reports how many times the collector called sql.Open.
func newFakePGCollector(t *testing.T) (*Postgres, *fakePG, func() int) {
	t.Helper()
	pg := &fakePG{}
	name := fmt.Sprintf("fakepg-%d", fakePGSeq.Add(1))
	sql.Register(name, pg)

	var opens atomic.Int64
	p := NewPostgres("fake://dsn")
	p.open = func(dsn string) (*sql.DB, error) {
		opens.Add(1)
		return sql.Open(name, dsn)
	}
	return p, pg, func() int { return int(opens.Load()) }
}

// --- change B -----------------------------------------------------------------

func TestPostgresOpensHandleOncePerCollectorNotPerSample(t *testing.T) {
	p, pg, opens := newFakePGCollector(t)

	const samples = 5
	for i := 0; i < samples; i++ {
		got, err := p.Collect(context.Background())
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		byKey := map[string]float64{}
		for _, s := range got {
			byKey[s.Key] = s.Value
		}
		if byKey["postgres.conns"] != 7 || byKey["postgres.conns_max"] != 100 {
			t.Fatalf("sample %d: got %v", i, byKey)
		}
	}

	if n := opens(); n != 1 {
		t.Errorf("sql.Open called %d times across %d samples, want 1 — the monitored database is paying for a new pool every interval", n, samples)
	}
	if n := pg.dialCount(); n != 1 {
		t.Errorf("driver dialled %d times across %d samples, want 1 — connection setup is being repeated per interval", n, samples)
	}
}

func TestPostgresHandleIsConfiguredForASingleConnection(t *testing.T) {
	p, _, _ := newFakePGCollector(t)
	if _, err := p.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.db == nil {
		t.Fatal("no long-lived handle retained after Collect")
	}
	if got := p.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1 — a monitoring probe must not open a pool against the monitored database", got)
	}
}

// A long-lived handle is only safe if database/sql actually re-dials when the
// monitored database restarts or drops the connection underneath us. It does:
// the pool discards a conn that reports driver.ErrBadConn and opens a new one,
// without the collector ever re-running sql.Open.
func TestPostgresReconnectsAfterDatabaseDropsTheConnection(t *testing.T) {
	p, pg, opens := newFakePGCollector(t)

	if _, err := p.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	dialsBefore := pg.dialCount()

	pg.mu.Lock()
	pg.failNext = 1 // the database went away between samples
	pg.mu.Unlock()

	got, err := p.Collect(context.Background())
	if err != nil {
		t.Fatalf("collector did not recover from a dropped connection: %v", err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["postgres.conns"] != 7 || byKey["postgres.conns_max"] != 100 {
		t.Fatalf("after reconnect got %v", byKey)
	}
	if n := pg.dialCount(); n <= dialsBefore {
		t.Errorf("dials = %d, want > %d — the pool did not re-dial after the connection died", n, dialsBefore)
	}
	if n := opens(); n != 1 {
		t.Errorf("sql.Open called %d times, want 1 — reconnection is the pool's job, not the collector's", n)
	}
}

// The mechanism that actually makes a long-lived handle safe, and the one the
// const block in postgres.go describes: database/sql asks the driver to validate
// a pooled connection on the way OUT of the pool, before the query, by calling
// driver.SessionResetter.ResetSession (driverConn.resetSession in
// database/sql/sql.go). pgx's stdlib *Conn implements it and pings the server
// whenever the connection has been idle for more than a second — which, at every
// push interval the mother can hand down, it always has.
//
// The test is built so that nothing else can save the sample: the stale
// connection's own query error is errServerGone, which database/sql does not
// retry. If the pool handed that connection back out unchecked, this Collect
// would fail. It succeeds only because the dead connection is caught and
// discarded at checkout and a fresh one is dialled.
func TestPostgresRecoversWhenTheHeldConnectionDiedWhileIdle(t *testing.T) {
	p, pg, opens := newFakePGCollector(t)

	if _, err := p.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	dialsBefore := pg.dialCount()

	pg.restart() // Supabase restarted between samples; the pooled conn is dead

	got, err := p.Collect(context.Background())
	if err != nil {
		t.Fatalf("collector did not survive a server restart between samples: %v", err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["postgres.conns"] != 7 || byKey["postgres.conns_max"] != 100 {
		t.Fatalf("after the server came back got %v", byKey)
	}
	if n := pg.dialCount(); n <= dialsBefore {
		t.Errorf("dials = %d, want > %d — the dead connection was reused instead of being replaced", n, dialsBefore)
	}
	if n := opens(); n != 1 {
		t.Errorf("sql.Open called %d times, want 1 — recovery is the pool's job, not the collector's", n)
	}
}

func TestPostgresHandleIsSafeUnderConcurrentCollect(t *testing.T) {
	p, _, opens := newFakePGCollector(t)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Collect(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := opens(); n != 1 {
		t.Errorf("sql.Open called %d times under concurrent Collect, want 1", n)
	}
}

// --- age caps -----------------------------------------------------------------

// poolDuration reads one of the pool's own age settings back off a *sql.DB.
// database/sql publishes no getter for either: Stats() exposes only
// MaxIdleTimeClosed / MaxLifetimeClosed, counts that need minutes of real wall
// clock to move off zero, which is no basis for a unit test. So this reads the
// fields the pool itself consults — DB.maxIdleTime and DB.maxLifetime, used by
// connectionCleanerRunLocked and driverConn.expired in database/sql/sql.go.
// Reading an unexported field through reflect is allowed; only Interface() and
// Set() are barred.
func poolDuration(t *testing.T, db *sql.DB, field string) time.Duration {
	t.Helper()
	f := reflect.ValueOf(db).Elem().FieldByName(field)
	if !f.IsValid() || f.Kind() != reflect.Int64 {
		t.Fatalf("database/sql no longer has a %s duration on *sql.DB — this test needs rewriting against whatever replaced it, not deleting", field)
	}
	return time.Duration(f.Int())
}

// Both caps are write-only from the process's point of view: nothing reads them
// back and no behaviour in any other test depends on them, so either
// SetConnMaxIdleTime or SetConnMaxLifetime can be deleted from handle() with
// every other test in this file still green — while the one connection this
// agent holds against Supabase quietly becomes immortal.
func TestPostgresHandleCapsIdleTimeAndConnectionAge(t *testing.T) {
	p, _, _ := newFakePGCollector(t)
	if _, err := p.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.db == nil {
		t.Fatal("no long-lived handle retained after Collect")
	}

	if got := poolDuration(t, p.db, "maxIdleTime"); got != pgConnMaxIdleTime {
		t.Errorf("ConnMaxIdleTime = %v, want %v — with a push interval longer than the cap, an all-but-unused connection would otherwise sit on the monitored database for the whole gap", got, pgConnMaxIdleTime)
	}
	if got := poolDuration(t, p.db, "maxLifetime"); got != pgConnMaxLifetime {
		t.Errorf("ConnMaxLifetime = %v, want %v — at the default 10s interval this is the only cap that can fire, and without it the agent pins one TCP connection to one pooler backend forever", got, pgConnMaxLifetime)
	}
}

// The two caps are not independent. With a single connection, whichever cap is
// shorter is the only one that can ever fire: if the idle cap were the longer of
// the two, the connection would always be retired by age before it could be
// reaped for idleness, and the idle cap would be unreachable at every push
// interval the mother can hand down (2s–3600s). Keeping idle < lifetime is what
// gives each cap its own reachable interval regime, which is the argument the
// const block makes.
func TestPostgresIdleCapIsReachableAlongsideTheLifetimeCap(t *testing.T) {
	if pgConnMaxIdleTime >= pgConnMaxLifetime {
		t.Fatalf("ConnMaxIdleTime (%v) must be shorter than ConnMaxLifetime (%v), or one of the two caps is dead code at every possible push interval", pgConnMaxIdleTime, pgConnMaxLifetime)
	}
}
