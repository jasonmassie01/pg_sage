package cases_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Integration (pr-01b): PR mode on real PostgreSQL with the fake code
// host and a fixture finding. The PR is recorded in sage.source_fix
// through agenttools (origin pg_sage), the advisory lock keeps racing
// cycles to one PR, and the followed state lands in pr_state.

const dbRepo = "acme/shop"

func prModePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "pr_mode")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	for run := 1; run <= 2; run++ {
		require.NoError(t, schema.Bootstrap(ctx, pool), "bootstrap run %d", run)
	}
	return pool
}

func migrationFinding(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(), `INSERT INTO sage.findings
	    (category, severity, object_type, object_identifier, title, detail,
	     recommendation, recommended_sql, rollback_sql)
	    VALUES ('migration_safety', 'warning', 'table', 'public.orders',
	     'Add a NOT VALID check on orders.total', '{}'::jsonb,
	     'Write it as a migration.', $1, $2) RETURNING id`,
		"ALTER TABLE public.orders ADD CONSTRAINT orders_total_ck CHECK (total >= 0) "+
			"NOT VALID;", "ALTER TABLE public.orders DROP CONSTRAINT orders_total_ck;").
		Scan(&id))
	return id
}

func dbHost() *codehosttest.Fake {
	host := codehosttest.New()
	host.AddFile(dbRepo, "db/migrate/0001_init.sql", "CREATE TABLE orders (total int);")
	return host
}

func dbPRMode(t *testing.T, pool *pgxpool.Pool, host codehost.Host) *cases.PRMode {
	t.Helper()
	tools := agenttools.New(pool, agenttools.Options{})
	m, err := cases.NewPRMode(cases.PRModeConfig{Repo: dbRepo, MigrationsPath: "db/migrate"},
		cases.PRModeDeps{Host: host, Store: cases.NewPRStore(pool),
			Recorder: agenttools.PRRecorder{Tools: tools}})
	require.NoError(t, err)
	return m
}

func dbCandidate(id int64) cases.PRCandidate {
	return cases.PRCandidate{FindingID: id, Kind: cases.PRKindMigration, RiskTier: "high",
		Title: "Add a NOT VALID check on orders.total",
		MigrationSQL: "ALTER TABLE public.orders ADD CONSTRAINT orders_total_ck CHECK " +
			"(total >= 0) NOT VALID;",
		RollbackSQL: "ALTER TABLE public.orders DROP CONSTRAINT orders_total_ck;"}
}

type prRow struct {
	Origin, Repo, State, Stage, URL string
	Number                          int
}

func readPRRow(t *testing.T, pool *pgxpool.Pool, id int64) prRow {
	t.Helper()
	var r prRow
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT origin, repo, pr_number,
	    pr_state, stage, pr_url FROM sage.source_fix WHERE finding_id = $1`, id).
		Scan(&r.Origin, &r.Repo, &r.Number, &r.State, &r.Stage, &r.URL))
	return r
}

func TestPRModeDB_OpensRecordsAndSkipsWhenCovered(t *testing.T) {
	pool := prModePool(t)
	id := migrationFinding(t, pool)
	host := dbHost()
	m := dbPRMode(t, pool, host)
	ctx := context.Background()
	out, err := m.Run(ctx, dbCandidate(id))
	require.NoError(t, err)
	require.Equal(t, cases.PROpened, out.Status)
	require.Equal(t, "db/migrate/0002_pg_sage_add_a_not_valid_check_on_orders_total.sql",
		host.Opened()[0].Files[0].Path)
	require.Equal(t, prRow{Origin: "pg_sage", Repo: dbRepo, State: "open",
		Stage: "pr_opened", URL: out.URL, Number: 1}, readPRRow(t, pool, id))
	for i := 0; i < 10; i++ { // CHECK-02 on the real store
		again, err := m.Run(ctx, dbCandidate(id))
		require.NoError(t, err)
		require.Equal(t, cases.PRCovered, again.Status)
	}
	require.Equal(t, 1, len(host.Opened()))
}

func TestPRModeDB_RacingCyclesOpenOnePR(t *testing.T) {
	pool := prModePool(t)
	id := migrationFinding(t, pool)
	host := dbHost()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := dbPRMode(t, pool, host) // one analyzer cycle each
			if _, err := m.Run(context.Background(), dbCandidate(id)); err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, len(host.Opened()), "the advisory lock and marker give one PR")
	var locks int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks
	    WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database
	      WHERE datname = current_database())`).Scan(&locks))
	require.Equal(t, 0, locks, "every lock is released")
}

func TestPRModeDB_OpenFailureRecordsNothing(t *testing.T) {
	pool := prModePool(t)
	id := migrationFinding(t, pool)
	host := dbHost()
	host.FailOpenPR(codehost.ErrForbidden)
	out, err := dbPRMode(t, pool, host).Run(context.Background(), dbCandidate(id))
	require.ErrorIs(t, err, codehost.ErrForbidden)
	require.Equal(t, cases.PRNotOpened, out.Status)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.source_fix WHERE finding_id = $1`, id).Scan(&n))
	require.Equal(t, 0, n)
}

func TestPRModeDB_FindingWithoutAChangeOpensNoPR(t *testing.T) {
	pool := prModePool(t)
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(), `INSERT INTO sage.findings
	    (category, severity, object_type, object_identifier, title, detail)
	    VALUES ('query_tuning', 'info', 'query', 'queryid:1', 't', '{}'::jsonb)
	    RETURNING id`).Scan(&id))
	host := dbHost()
	out, err := dbPRMode(t, pool, host).Run(context.Background(), dbCandidate(id))
	require.NoError(t, err)
	require.Equal(t, cases.PRNotOpened, out.Status)
	require.Contains(t, out.Note, "no source-fix packet")
	require.Empty(t, host.Opened())
}

func TestPRModeDB_FollowRecordsDeclined(t *testing.T) {
	pool := prModePool(t)
	id := migrationFinding(t, pool)
	host := dbHost()
	m := dbPRMode(t, pool, host)
	ctx := context.Background()
	_, err := m.Run(ctx, dbCandidate(id))
	require.NoError(t, err)
	host.SetState(dbRepo, codehost.PRStatus{Number: 1, State: codehost.PRClosed})
	out, err := m.Follow(ctx, dbCandidate(id))
	require.NoError(t, err)
	require.Equal(t, "declined in acme/shop#1", out.Note)
	require.Equal(t, "closed", readPRRow(t, pool, id).State)
	again, err := m.Run(ctx, dbCandidate(id))
	require.NoError(t, err)
	require.Equal(t, cases.PRDeclined, again.Status)
	require.Equal(t, 1, len(host.Opened()))
	require.ErrorContains(t, cases.NewPRStore(pool).SetPRState(ctx, id+999, "merged"),
		"no PR opened by pg_sage")
}
