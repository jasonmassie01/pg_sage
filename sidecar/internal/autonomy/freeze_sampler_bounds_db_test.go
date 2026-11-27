package autonomy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-11c on real PostgreSQL: the freeze sampler's reading runs under
// the configured statement and lock timeouts, so a catalog lock held by
// DDL ends the reading within the lock timeout instead of hanging it.

// settingsTx reports the reading transaction's own bounds just before it
// commits.
type settingsTx struct {
	pgx.Tx
	got *[2]string
}

func (tx settingsTx) Commit(ctx context.Context) error {
	if err := tx.QueryRow(ctx, `SELECT current_setting('statement_timeout'),
		current_setting('lock_timeout')`).Scan(&tx.got[0], &tx.got[1]); err != nil {
		return err
	}
	return tx.Tx.Commit(ctx)
}

type settingsBeginner struct {
	conn *pgx.Conn
	got  [2]string
}

func (b *settingsBeginner) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	tx, err := b.conn.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return settingsTx{Tx: tx, got: &b.got}, nil
}

func samplerTestConn(t *testing.T) *pgx.Conn {
	t.Helper()
	requireAutonomyDB(t) // a live, bootstrapped server
	return samplerConnOn(t, testdb.SkipUnlessLive(t))
}

// samplerConnOn is one connection to the database dsn names.
func samplerConnOn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// lockedCatalogDSN is a database on the exclusive server for a test that
// holds its catalog lock (holdCatalogLock).
func lockedCatalogDSN(t *testing.T) string {
	t.Helper()
	return exclusiveAutonomyDSN(t, "holds pg_class ACCESS EXCLUSIVE; another "+
		"package's DDL makes the next BEGIN wait on it")
}

// liveDeadline bounds a test that reads under a held catalog lock: a
// reading left unbounded fails at the deadline instead of hanging the
// package until go test's own timeout.
func liveDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// holdCatalogLock takes ACCESS EXCLUSIVE on dsn's pg_class, which the
// horizon read scans, as DDL waiting on a catalog would; release ends it.
//
// Its tests run on the exclusive server (lockedCatalogDSN): other packages'
// DDL can overflow a backend's invalidation queue, and the cache rebuild
// that follows at the next BEGIN waits on this lock before any bound is
// set, so the reading ends at the context deadline instead of a timeout.
func holdCatalogLock(t *testing.T, dsn string) (release func()) {
	t.Helper()
	holder := samplerConnOn(t, dsn)
	tx, err := holder.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	if _, err := tx.Exec(context.Background(),
		"LOCK TABLE pg_catalog.pg_class IN ACCESS EXCLUSIVE MODE"); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("lock pg_class: %v", err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = tx.Rollback(context.Background())
		}
	}
	t.Cleanup(release)
	return release
}

func TestFreezeSamplerLiveReadRunsUnderItsBounds(t *testing.T) {
	db := &settingsBeginner{conn: samplerTestConn(t)}
	sampler := NewFreezeSampler(db, 0, 0)
	sampler.bounds = catalogread.Timeouts{Statement: 5 * time.Second, Lock: 2 * time.Second}

	reading, err := sampler.Read(context.Background())

	if err != nil {
		t.Fatalf("Read on an idle server: %v", err)
	}
	if len(reading.Horizons) == 0 || reading.At.IsZero() {
		t.Fatalf("reading = %d horizons at %v, want the catalog's horizons",
			len(reading.Horizons), reading.At)
	}
	if db.got != [2]string{"5s", "2s"} {
		t.Fatalf("reading ran with statement_timeout, lock_timeout = %q, want 5s, 2s", db.got)
	}
}

// Error propagation and state transition: a held catalog lock ends the
// reading at the lock timeout; once it is released, the next reading
// succeeds and advances the rates from the last good counters.
func TestFreezeSamplerLiveLockTimeoutThenRecovers(t *testing.T) {
	dsn := lockedCatalogDSN(t)
	sampler := NewFreezeSampler(samplerConnOn(t, dsn), 0, 0)
	sampler.bounds = catalogread.Timeouts{Statement: 5 * time.Second, Lock: 200 * time.Millisecond}
	ctx := liveDeadline(t)
	if _, err := sampler.Read(ctx); err != nil { // warms the connection's caches
		t.Fatalf("first Read: %v", err)
	}
	release := holdCatalogLock(t, dsn)

	started := time.Now()
	_, err := sampler.Read(ctx)
	took := time.Since(started)

	var pgErr *pgconn.PgError
	if err == nil || !strings.Contains(err.Error(), "lock timeout") ||
		!errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("Read under a held pg_class lock = %v, want a lock timeout (55P03)", err)
	}
	if took > 2*time.Second {
		t.Fatalf("lock timeout took %v, want under 2s", took)
	}
	release()
	fresh, err := sampler.Read(ctx)
	if err != nil || !fresh.rates.known || len(fresh.Horizons) == 0 {
		t.Fatalf("Read after release = %d horizons, rates %+v (%v), want known rates",
			len(fresh.Horizons), fresh.rates, err)
	}
}

// The custodian takes its sampler's bounds from WithBloatBounds and
// proposes nothing in a cycle whose reading timed out.
func TestFreezeSamplerCustodianProposesNothingOnLockTimeout(t *testing.T) {
	dsn := lockedCatalogDSN(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns, cfg.MinConns = 1, 1 // one warm connection: none opens under the lock
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	c := NewPostgresFreezeCustodian(pool, "testdb", 25).WithBloatBounds(0,
		catalogread.Timeouts{Statement: 5 * time.Second, Lock: 200 * time.Millisecond})
	ctx := liveDeadline(t)
	if _, err := c.Sampler().Read(ctx); err != nil {
		t.Fatalf("warm-up Read: %v", err)
	}
	holdCatalogLock(t, dsn)

	started := time.Now()
	proposals, err := c.Scan(ctx)

	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("Scan under a held pg_class lock = %v, want the lock timeout", err)
	}
	if len(proposals) != 0 {
		t.Fatalf("Scan proposed %d actions on a timed-out reading, want none", len(proposals))
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("Scan took %v under the lock, want under 2s", took)
	}
}

// The shipped bounds have a statement timeout (500 ms) far shorter than the
// lock timeout (30 s): a held catalog lock then ends the reading at the
// statement timeout, and the error names that bound.
func TestFreezeSamplerLiveStatementTimeoutUnderAHeldLock(t *testing.T) {
	dsn := lockedCatalogDSN(t)
	sampler := NewFreezeSampler(samplerConnOn(t, dsn), 0, 0)
	sampler.bounds = catalogread.Timeouts{Statement: 200 * time.Millisecond, Lock: 5 * time.Second}
	ctx := liveDeadline(t)
	if _, err := sampler.Read(ctx); err != nil { // warms the connection's caches
		t.Fatalf("first Read: %v", err)
	}
	holdCatalogLock(t, dsn)

	started := time.Now()
	_, err := sampler.Read(ctx)
	took := time.Since(started)

	var pgErr *pgconn.PgError
	if err == nil || !strings.Contains(err.Error(), "freeze age read: statement timeout") ||
		!errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("Read under a held pg_class lock = %v, want a statement timeout (57014)", err)
	}
	if took > 2*time.Second {
		t.Fatalf("statement timeout took %v, want under 2s", took)
	}
}
