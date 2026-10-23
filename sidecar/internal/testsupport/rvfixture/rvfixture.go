// Package rvfixture builds the Change Review fixture schema rv on a live
// test server, identically on PostgreSQL 14-18, for the impact engine's
// and the receipts' tests (CHECK-IMP-01).
//
// The schema and its rows are built once per package process into the
// package's fixture database, which then serves as the template every
// test copies (CREATE DATABASE ... TEMPLATE). Each copy runs the workload
// itself, over the extended protocol, because pg_stat_statements and the
// cumulative statistics are kept per database:
//
//	Q1 SELECT email FROM rv.rv_orders WHERE id = $1          x 2000
//	Q2 UPDATE rv.rv_orders SET status = $1 WHERE id = $2     x  500
//	Q3 SELECT count(*) FROM rv.rv_orders WHERE created_at > $1 x 50
//
// and then records two querystore.Record samples of them 5 minutes apart,
// a 'tables' snapshot two hours old and runs ANALYZE.
package rvfixture

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Fixture sizes and names.
const (
	Schema      = "rv"
	Orders      = "rv.rv_orders"
	Items       = "rv.rv_items"
	CreatedIdx  = "rv.rv_orders_created_idx"
	OrdersRows  = 200_000
	ItemsRows   = 400_000
	Q1Calls     = 2000
	Q2Calls     = 500
	Q3Calls     = 50
	SampleGap   = 5 * time.Minute
	SnapshotAge = 2 * time.Hour
)

// Workload statement texts, exactly as the fixture sends them. Q1 carries
// sqlcommenter tags.
const (
	Q1Text = "SELECT email FROM rv.rv_orders WHERE id = $1 " +
		"/*application='billing-api',route='GET%20/orders/:id'*/"
	Q2Text = "UPDATE rv.rv_orders SET status = $1 WHERE id = $2"
	Q3Text = "SELECT count(*) FROM rv.rv_orders WHERE created_at > $1"
)

// Q3Since is Q3's argument: the last day of rv_orders, so Q3 scans
// rv_orders_created_idx.
var Q3Since = time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)

// Query is one workload statement as pg_stat_statements recorded it.
type Query struct {
	ID    int64
	Text  string
	Calls int64
}

// Fixture is one test's own copy of the rv database.
type Fixture struct {
	DSN  string
	Name string
	Pool *pgxpool.Pool
	Q1   Query
	Q2   Query
	Q3   Query
	// SampledFrom and SampledTo are the capture times of the two
	// query_store samples (calls 0 at SampledFrom).
	SampledFrom time.Time
	SampledTo   time.Time
	t           testing.TB
}

// RatePerSecond is a workload query's calls between the two samples per
// second of the gap.
func (q Query) RatePerSecond() float64 { return float64(q.Calls) / SampleGap.Seconds() }

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
)

// New returns a fresh copy of the rv database with the workload run, its
// samples recorded and ANALYZE done. It skips without a live test server.
func New(t testing.TB) *Fixture {
	t.Helper()
	base := testdb.SkipUnlessLive(t)
	templateOnce.Do(func() { templateName, templateErr = buildTemplate(base) })
	if templateErr != nil {
		t.Fatalf("build rv fixture template: %v", templateErr)
	}
	dsn := testdb.CloneDatabase(t, "rv", templateName)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect rv fixture: %v", err)
	}
	t.Cleanup(pool.Close)
	name, err := testdb.DatabaseName(dsn)
	if err != nil {
		t.Fatalf("rv fixture name: %v", err)
	}
	f := &Fixture{DSN: dsn, Name: name, Pool: pool, t: t}
	if err := f.prepare(ctx); err != nil {
		t.Fatalf("prepare rv fixture: %v", err)
	}
	return f
}

// buildTemplate builds rv into the package's fixture database and leaves
// no session connected to it.
func buildTemplate(dsn string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name, err := testdb.DatabaseName(dsn)
	if err != nil {
		return "", err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("connect template: %w", err)
	}
	defer pool.Close()
	if err := Build(ctx, pool); err != nil {
		return "", err
	}
	return name, nil
}

// Build creates the sage schema, HypoPG (when the server has it) and
// schema rv with its rows and index in pool's database, then VACUUM
// ANALYZEs rv. It does not run the workload.
func Build(ctx context.Context, pool *pgxpool.Pool) error {
	if err := schema.Bootstrap(ctx, pool); err != nil {
		return fmt.Errorf("bootstrap sage schema: %w", err)
	}
	for _, stmt := range buildStatements {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("build rv: %s: %w", stmt, err)
		}
	}
	return nil
}

var buildStatements = []string{
	`DO $$ BEGIN
	   IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'hypopg') THEN
	     CREATE EXTENSION IF NOT EXISTS hypopg;
	   END IF;
	 END $$`,
	`CREATE SCHEMA rv`,
	`CREATE TABLE rv.rv_orders (id bigserial PRIMARY KEY, email text, status text,
	   code varchar(10), created_at timestamptz, customer_id int)`,
	`INSERT INTO rv.rv_orders (email, status, code, created_at, customer_id)
	 SELECT 'user' || i || '@example.com',
	        (ARRAY['new','paid','shipped','void','open'])[i % 5 + 1],
	        'C' || (i % 1000),
	        timestamptz '2026-01-01 00:00:00+00' + i * interval '1 minute',
	        i % 5000 + 1
	   FROM generate_series(1, 200000) AS i`,
	`CREATE INDEX rv_orders_created_idx ON rv.rv_orders (created_at)`,
	`CREATE TABLE rv.rv_items (id bigserial PRIMARY KEY, order_id int, qty int)`,
	`INSERT INTO rv.rv_items (order_id, qty)
	 SELECT i % 200000 + 1, i % 50 FROM generate_series(1, 400000) AS i`,
	`VACUUM ANALYZE rv.rv_orders`,
	`VACUUM ANALYZE rv.rv_items`,
}

// prepare runs the workload on one connection, flushes its statistics,
// records the samples and the snapshot and runs ANALYZE.
func (f *Fixture) prepare(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, f.DSN)
	if err != nil {
		return fmt.Errorf("connect workload session: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := runWorkload(ctx, conn); err != nil {
		return err
	}
	if err := testdb.FlushStats(ctx, conn); err != nil {
		return err
	}
	for _, q := range []struct {
		dst  *Query
		text string
	}{{&f.Q1, Q1Text}, {&f.Q2, Q2Text}, {&f.Q3, Q3Text}} {
		if *q.dst, err = statement(ctx, f.Pool, q.text); err != nil {
			return err
		}
	}
	if err := f.recordSamples(ctx); err != nil {
		return err
	}
	if err := f.recordSnapshot(ctx); err != nil {
		return err
	}
	_, err = f.Pool.Exec(ctx, "ANALYZE rv.rv_orders, rv.rv_items")
	return err
}

func runWorkload(ctx context.Context, conn *pgx.Conn) error {
	batch := &pgx.Batch{}
	for i := 1; i <= Q1Calls; i++ {
		batch.Queue(Q1Text, int64(i))
	}
	for i := 1; i <= Q2Calls; i++ {
		batch.Queue(Q2Text, "paid", int64(i))
	}
	for i := 0; i < Q3Calls; i++ {
		batch.Queue(Q3Text, Q3Since)
	}
	if err := conn.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("run rv workload: %w", err)
	}
	return nil
}

// statement finds text's pg_stat_statements entry in this database.
func statement(ctx context.Context, pool *pgxpool.Pool, text string) (Query, error) {
	q := Query{Text: text}
	err := pool.QueryRow(ctx, `SELECT s.queryid, s.calls::int8
		FROM pg_stat_statements s
		WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND s.query = $1`, text).Scan(&q.ID, &q.Calls)
	if err != nil {
		return q, fmt.Errorf("find workload statement %q: %w", text, err)
	}
	return q, nil
}
