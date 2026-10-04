package agenttools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: hypothetical indexes are session-local and
// WhatIfIndex holds no state between calls; the optimizer's HypoPG session
// tests cover session isolation.

// requireHypoPG installs HypoPG in the fixture database or skips.
func requireHypoPG(t *testing.T, f *fixture) {
	t.Helper()
	var available bool
	f.scalar(&available, `SELECT EXISTS (SELECT 1 FROM pg_available_extensions
		WHERE name = 'hypopg')`)
	if !available {
		t.Skip("HypoPG server extension unavailable; what-if integration not verified")
	}
	f.exec("CREATE EXTENSION IF NOT EXISTS hypopg")
}

// singleConnPool is a one-connection pool on the fixture database, so a
// check after WhatIfIndex runs on the very session that held the
// hypothetical index.
func singleConnPool(t *testing.T, f *fixture) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	require.NoError(t, err)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// itemsWorkload creates <schema>.items (50k rows, 10k distinct category
// values) and records the filter query in pg_stat_statements.
func itemsWorkload(t *testing.T, f *fixture) (string, int64) {
	t.Helper()
	items := f.q("items")
	f.exec("CREATE TABLE "+items+" (id int, category int, note text)",
		"INSERT INTO "+items+" SELECT i, i % 10000, md5(i::text) "+
			"FROM generate_series(1, 50000) i",
		"ANALYZE "+items)
	var id int64
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		for i := 0; i < 3; i++ {
			f.execArgs("SELECT id FROM "+items+" WHERE category = $1", 42)
		}
		var ok bool
		if id, ok = f.statementID("WHERE category = $1"); !ok {
			return []string{"filter query missing from pg_stat_statements"}
		}
		return nil
	})
	return items, id
}

func hypotheticalCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM hypopg_list_indexes").Scan(&n))
	return n
}

func TestWhatIfIndexVerifiesHelpfulIndex(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	requireHypoPG(t, f)
	items, qid := itemsWorkload(t, f)
	pool := singleConnPool(t, f)
	ctx, cancel := context.WithTimeout(f.ctx, 60*time.Second)
	defer cancel()
	res, err := New(pool, Options{}).WhatIfIndex(ctx, WhatIfRequest{
		DDL: "CREATE INDEX ON " + items + " (category)", QueryIDs: []QueryID{QueryID(qid)}})
	require.NoError(t, err)
	require.True(t, res.Available, "HypoPG installed but reported unavailable: %s", res.Note)
	require.Equal(t, "verified", res.Verdict, res.Reason)
	require.Greater(t, res.ImprovementPct, 20.0)
	require.LessOrEqual(t, res.ImprovementPct, 100.0)
	require.Positive(t, res.SizeBytes)
	require.Equal(t, 1, res.Measured)
	require.Equal(t, 0, res.Failed)
	require.Equal(t, []QueryID{QueryID(qid)}, res.QueryIDs)
	require.Equal(t, int64(0), hypotheticalCount(t, ctx, pool), "hypothetical index leaked")
	require.Equal(t, int64(0), f.count("pg_indexes WHERE schemaname = $1 AND tablename = $2",
		f.schema, "items"), "what-if created a real index")
}

// An index no workload query can use is measured but not verified.
func TestWhatIfIndexUselessIndexIsNotVerified(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	requireHypoPG(t, f)
	items, qid := itemsWorkload(t, f)
	res, err := New(f.pool, Options{}).WhatIfIndex(f.ctx, WhatIfRequest{
		DDL: "CREATE INDEX ON " + items + " (note)", QueryIDs: []QueryID{QueryID(qid)}})
	require.NoError(t, err)
	require.True(t, res.Available)
	require.NotEqual(t, "verified", res.Verdict, "an unused index was verified")
	require.NotEmpty(t, res.Reason, "a non-verified verdict says why")
	require.Less(t, res.ImprovementPct, 20.0)
}

func TestWhatIfIndexAcceptsUniqueIndex(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	requireHypoPG(t, f)
	items, qid := itemsWorkload(t, f)
	res, err := New(f.pool, Options{}).WhatIfIndex(f.ctx, WhatIfRequest{
		DDL:      "CREATE UNIQUE INDEX items_id_key ON " + items + " (id)",
		QueryIDs: []QueryID{QueryID(qid)}})
	// HypoPG versions differ on UNIQUE; the shape itself must be accepted.
	if err != nil {
		require.False(t, errors.Is(err, ErrInvalid), "CREATE UNIQUE INDEX refused: %v", err)
	} else {
		require.True(t, res.Available)
		require.Equal(t, 1, res.Measured+res.Failed, "the one query was evaluated")
	}
	require.False(t, f.relationExists(f.q("items_id_key")), "a real index was created")
}

func TestWhatIfIndexRejectsNonIndexDDL(t *testing.T) {
	f := newFixture(t)
	items := f.q("items")
	f.exec("CREATE TABLE " + items + " (id int, category int)")
	tools := New(f.pool, Options{})
	for _, ddl := range []string{
		"", "   ", "DROP TABLE " + items, "CREATE TABLE " + f.q("x") + " ()",
		"ALTER TABLE " + items + " ADD COLUMN c int",
		"CREATE INDEX i ON " + items + " (category); DROP TABLE " + items,
		"CREATE INDEX i ON " + items + " (category);DROP TABLE " + items + ";",
		"SELECT 1",
		"DROP INDEX " + f.q("i"),
		"CREATE INDEX i ON " + items + " (category) /* ok */ ; SELECT pg_sleep(10)",
	} {
		_, err := tools.WhatIfIndex(f.ctx, WhatIfRequest{DDL: ddl, QueryIDs: []QueryID{1}})
		require.ErrorIs(t, err, ErrInvalid, "DDL %q", ddl)
	}
	require.True(t, f.relationExists(items), "table dropped through WhatIfIndex")
	require.False(t, f.relationExists(f.q("x")), "table created through WhatIfIndex")
	require.Equal(t, int64(0), f.count("pg_indexes WHERE schemaname = $1", f.schema))
}

func TestWhatIfIndexWithoutHypoPGIsUnavailableNotError(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	bare := extraDatabase(t, f.ctx, "nohypo")
	mustExec(t, f, bare, "DROP EXTENSION IF EXISTS hypopg")
	mustExec(t, f, bare, "CREATE TABLE public.items (id int, category int)")
	mustExec(t, f, bare, "SELECT id FROM public.items WHERE category = $1", 42)
	var qid int64
	err := bare.QueryRow(f.ctx, `SELECT queryid FROM pg_stat_statements s
		JOIN pg_database d ON d.oid = s.dbid
		WHERE d.datname = current_database() AND strpos(query, 'category = $1') > 0
		LIMIT 1`).Scan(&qid)
	if errors.Is(err, pgx.ErrNoRows) {
		qid = 1 // evicted by a concurrent reset: availability does not depend on it
	} else {
		require.NoError(t, err)
	}
	res, err := New(bare, Options{}).WhatIfIndex(f.ctx, WhatIfRequest{
		DDL: "CREATE INDEX ON public.items (category)", QueryIDs: []QueryID{QueryID(qid)}})
	require.NoError(t, err, "missing HypoPG is reported in the result, not as an error")
	require.False(t, res.Available)
	require.NotEmpty(t, res.Note, "the result says why no what-if ran")
	require.Equal(t, 0, res.Measured)
	require.Equal(t, int64(0), res.SizeBytes)
	var real int64
	require.NoError(t, bare.QueryRow(f.ctx, `SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'public' AND tablename = 'items'`).Scan(&real))
	require.Equal(t, int64(0), real, "fallback created a real index")
}

func TestWhatIfIndexInvalidDDLCheckedBeforeAvailability(t *testing.T) {
	f := newFixture(t)
	bare := extraDatabase(t, f.ctx, "nohypo_inv")
	mustExec(t, f, bare, "DROP EXTENSION IF EXISTS hypopg")
	_, err := New(bare, Options{}).WhatIfIndex(f.ctx, WhatIfRequest{DDL: "DROP TABLE x"})
	require.ErrorIs(t, err, ErrInvalid, "shape errors are reported even without HypoPG")
}
