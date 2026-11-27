//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// observability-06: the SRE backlog read against real sage.sre_* tables.

// No concurrent access tests here: one refresher reads one database at a
// time; scrape concurrency is covered by the refresher's own tests.

func sreMetricsPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// bindSREDatabase binds a new database under a unique runtime key and
// returns the key and the binding's identity.
func bindSREDatabase(t *testing.T, ctx context.Context, p *pgxpool.Pool) (string, string,
	string) {
	t.Helper()
	key := fmt.Sprintf("obs06:%d", time.Now().UnixNano())
	var dep, db string
	err := p.QueryRow(ctx, `WITH d AS (
		INSERT INTO sage.sre_deployments (deployment_id) VALUES (gen_random_uuid())
		ON CONFLICT DO NOTHING RETURNING deployment_id)
		SELECT deployment_id::text FROM d
		UNION ALL SELECT deployment_id::text FROM sage.sre_deployments LIMIT 1`).Scan(&dep)
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	err = p.QueryRow(ctx, `INSERT INTO sage.sre_database_bindings
		(deployment_id, database_id, runtime_key, identity_strength, cluster_epoch)
		VALUES ($1, gen_random_uuid(), $2, 'configured', 'unknown')
		RETURNING database_id::text`, dep, key).Scan(&db)
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	return key, dep, db
}

// seedInvestigation inserts one investigation in state, created ageSQL
// ago and last updated updatedSQL ago (SQL intervals, the server's clock).
func seedInvestigation(t *testing.T, ctx context.Context, p *pgxpool.Pool, dep, db,
	state, ageSQL, updatedSQL string) {
	t.Helper()
	_, err := p.Exec(ctx, `INSERT INTO sage.sre_investigations
		(deployment_id, database_id, id, source_case_id, trigger_kind,
		 trigger_fingerprint, state, created_at, updated_at, expires_at)
		VALUES ($1, $2, gen_random_uuid(), 'case:obs06', 'lock',
		 sha256(gen_random_uuid()::text::bytea), $3,
		 now() - $4::interval, now() - $5::interval, now() + interval '1 day')`,
		dep, db, state, ageSQL, updatedSQL)
	if err != nil {
		t.Fatalf("seed %s investigation: %v", state, err)
	}
}

// Integration: live states count whatever their age; terminal states only
// within the last 24 hours; another binding's rows never leak in.
func TestSREBacklogReadCountsByState(t *testing.T) {
	withMetricsRegistry(t)
	p, ctx := sreMetricsPool(t)
	key, dep, db := bindSREDatabase(t, ctx, p)
	seedInvestigation(t, ctx, p, dep, db, "queued", "3 hours", "3 hours")
	seedInvestigation(t, ctx, p, dep, db, "queued", "30 hours", "30 hours")
	seedInvestigation(t, ctx, p, dep, db, "needs_evidence", "1 hour", "1 hour")
	seedInvestigation(t, ctx, p, dep, db, "concluded", "3 hours", "2 hours")
	seedInvestigation(t, ctx, p, dep, db, "failed", "26 hours", "25 hours")
	_, otherDep, otherDB := bindSREDatabase(t, ctx, p)
	seedInvestigation(t, ctx, p, otherDep, otherDB, "queued", "50 hours", "50 hours")

	m := newMetricsRefresher("orders", fixedInterval(time.Hour),
		sreMetricsSource(controlBacklogReader(p, key)))
	processMetrics.register(m)
	m.refresh(ctx)
	if !m.snapshot().ok[metricsSourceSRE] {
		t.Fatal("SRE source refresh failed against a live store")
	}
	out := renderSRE(processMetrics.views()...)
	for _, want := range []string{
		`pg_sage_sre_investigations{database="orders",state="queued"} 2`,
		`pg_sage_sre_investigations{database="orders",state="needs_evidence"} 1`,
		`pg_sage_sre_investigations{database="orders",state="concluded"} 1`,
		`pg_sage_sre_investigations{database="orders",state="collecting"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, grepLines(out, "sre_investigation"))
		}
	}
	if strings.Contains(out, `state="failed"`) {
		t.Errorf("a failed run 25 hours old is exported:\n%s",
			grepLines(out, "sre_investigation"))
	}
	age := sreSampleValue(t, out,
		`pg_sage_sre_investigation_oldest_open_age_seconds{database="orders"}`)
	if age < 30*3600-60 || age > 30*3600+60 {
		t.Errorf("oldest open age = %v, want about 30h (this binding's oldest queued)", age)
	}
}

// Boundary: a terminal run updated 24 h minus 1 s ago is counted; one
// updated 24 h plus 1 s ago is not.
func TestSREBacklogReadTerminalWindowEdges(t *testing.T) {
	p, ctx := sreMetricsPool(t)
	key, dep, db := bindSREDatabase(t, ctx, p)
	seedInvestigation(t, ctx, p, dep, db, "expired", "2 days",
		"23 hours 59 minutes 59 seconds")
	seedInvestigation(t, ctx, p, dep, db, "cancelled", "2 days", "24 hours 1 second")
	got, err := readSREBacklog(ctx, p, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.byState["expired"] != 1 || got.byState["cancelled"] != 0 || len(got.byState) != 1 {
		t.Fatalf("byState = %v, want expired 1 only", got.byState)
	}
	if !got.oldestOpen.IsZero() {
		t.Fatalf("oldest open = %v with only terminal runs, want none", got.oldestOpen)
	}
}

// Nil/empty: a binding with no investigations reads as an empty backlog,
// and an unknown runtime key is not an error.
func TestSREBacklogReadEmpty(t *testing.T) {
	p, ctx := sreMetricsPool(t)
	key, _, _ := bindSREDatabase(t, ctx, p)
	for _, k := range []string{key, "obs06:never-bound"} {
		got, err := readSREBacklog(ctx, p, k)
		if err != nil || len(got.byState) != 0 || !got.oldestOpen.IsZero() {
			t.Fatalf("%s: read = %+v, %v; want an empty backlog", k, got, err)
		}
	}
}

// Error propagation: a read on a cancelled context fails with the cause.
func TestSREBacklogReadCancelled(t *testing.T) {
	p, ctx := sreMetricsPool(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := readSREBacklog(cctx, p, "obs06:any"); err == nil ||
		!strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("cancelled read = %v, want context canceled", err)
	}
}

// Integration and state transition: a runtime whose investigator runs
// reads its own binding (the runtime key from its database id) from its
// control pool, not from the monitored database.
func TestRuntimeMetricsRefresherReadsTheRuntimesBacklog(t *testing.T) {
	withMetricsRegistry(t)
	control, ctx := sreMetricsPool(t)
	monitored, err := pgxpool.New(ctx,
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	t.Cleanup(monitored.Close)
	id := int(time.Now().UnixNano()%1_000_000_000) + 1_000_000_000
	key, dep, db := bindSREDatabase(t, ctx, control)
	if _, err := control.Exec(ctx, `UPDATE sage.sre_database_bindings
		SET runtime_key = $1 WHERE runtime_key = $2`, fmt.Sprintf("db:%d", id),
		key); err != nil {
		t.Fatalf("rekey binding: %v", err)
	}
	seedInvestigation(t, ctx, control, dep, db, "queued", "10 minutes", "10 minutes")
	seedInvestigation(t, ctx, control, dep, db, "collecting", "5 minutes", "1 minute")
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: "orders", Pool: monitored,
		ControlPool: control, DatabaseID: id}, cfg: config.DefaultConfig(),
		sre: liveTestCoordinator(t, control, fmt.Sprintf("db:%d", id))}
	m := newRuntimeMetricsRefresher(rt)
	m.refresh(ctx)
	s := m.snapshot()
	if !s.ok[metricsSourceSRE] || s.sre == nil {
		t.Fatalf("SRE source ok=%v sre=%+v, want a backlog read", s.ok[metricsSourceSRE],
			s.sre)
	}
	if s.sre.byState["queued"] != 1 || s.sre.byState["collecting"] != 1 ||
		len(s.sre.byState) != 2 {
		t.Fatalf("backlog = %v, want this runtime's queued 1 and collecting 1",
			s.sre.byState)
	}
}

// liveTestCoordinator is a coordinator on the live control store; the
// refresher only needs it to exist.
func liveTestCoordinator(t *testing.T, p *pgxpool.Pool, key string) *sre.Coordinator {
	t.Helper()
	store, err := sre.NewPostgresStore(p, sre.DefaultLimits())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: store, Runner: chainRunner{},
		Config: sre.DefaultCoordinatorConfig(key), LogFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return coord
}
