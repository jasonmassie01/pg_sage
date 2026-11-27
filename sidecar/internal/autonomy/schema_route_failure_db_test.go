package autonomy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// migration-08 on real PostgreSQL: an FK index whose table is held under
// ACCESS EXCLUSIVE fails its CREATE INDEX CONCURRENTLY with lock_timeout
// every cycle. It is recorded once in sage.decision over three cycles, and
// the other tables' FK indexes are still built. No concurrent access test
// here: the parallel-scan guard is pinned in schemaguard with a fake ledger.

// fkScopedDetector returns the missing-FK-index invariants of one schema,
// so other tests' tables do not join the scan.
type fkScopedDetector struct {
	detector postgresSchemaDetector
	schema   string
}

func (d fkScopedDetector) Detect(ctx context.Context) ([]schemaguard.Invariant, error) {
	items, err := d.detector.detectMissingFKIndexes(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]schemaguard.Invariant, 0, len(items))
	for _, item := range items {
		if item.Schema == d.schema {
			result = append(result, item)
		}
	}
	return result, nil
}

// ddlRouter runs each proposal's DDL on its own connection under a short
// lock_timeout, the way the executor bounds a CONCURRENTLY build.
type ddlRouter struct {
	pool  *pgxpool.Pool
	mu    sync.Mutex
	calls map[string]int
}

func (r *ddlRouter) Route(ctx context.Context, proposal Proposal) error {
	return r.RouteVerifiedIndex(ctx, proposal, "", nil)
}

func (r *ddlRouter) RouteVerifiedIndex(
	ctx context.Context, proposal Proposal, _ string, _ []int64,
) error {
	r.mu.Lock()
	r.calls[proposal.TargetObjects[0]]++
	r.mu.Unlock()
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire DDL connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET lock_timeout = '200ms'"); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET lock_timeout") }()
	if _, err := conn.Exec(ctx, proposal.SQL); err != nil {
		return fmt.Errorf("build index for %v: %w", proposal.TargetObjects, err)
	}
	return nil
}

func (r *ddlRouter) callsFor(target string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[target]
}

func createRouteFailureFixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	schemaName := fmt.Sprintf("mig08_%d", time.Now().UnixNano())
	statements := []string{"CREATE SCHEMA " + schemaName,
		"CREATE TABLE " + schemaName + ".parent (id bigint PRIMARY KEY)"}
	for _, table := range []string{"a_hot", "b_cold", "c_cold"} {
		statements = append(statements, "CREATE TABLE "+schemaName+"."+table+
			" (id bigint PRIMARY KEY, parent_id bigint REFERENCES "+schemaName+
			".parent(id))")
	}
	execAll(t, pool, statements...)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
	})
	return schemaName
}

// holdAccessExclusive keeps table locked until the test ends.
func holdAccessExclusive(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock %s: %v", table, err)
	}
}

func schemaGuardRowsFor(t *testing.T, pool *pgxpool.Pool, identity string) int {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.decision
		WHERE feature = 'schema_guard' AND evidence->>'invariant_key' = $1`,
		identity).Scan(&count)
	if err != nil {
		t.Fatalf("count schema guard rows: %v", err)
	}
	return count
}

func TestBlockedFKIndexIsRecordedOnceAndOthersAreRouted(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	schemaName := createRouteFailureFixture(t, pool)
	detector := fkScopedDetector{newPostgresSchemaDetector(pool, nil), schemaName}
	detected, err := detector.Detect(ctx)
	if err != nil || len(detected) != 3 {
		t.Fatalf("detected %d FK invariants (err %v), want 3", len(detected), err)
	}
	hot := schemaName + ".a_hot"
	var identity string
	for _, item := range detected {
		if item.Target() == hot {
			identity = schemaguard.InvariantIdentity(item)
		}
	}
	holdAccessExclusive(t, pool, hot)
	router := &ddlRouter{pool: pool, calls: map[string]int{}}
	policyConfig := schemaguard.DefaultPolicy()
	policyConfig.AllowFKIndexApply = true
	custodian := schemaguard.NewCustodian(detector, postgresSchemaContractSource{pool},
		postgresSchemaHistorySource{pool}, schemaRemediationRouter{database: "testdb",
			router: router, verifiedIndexes: router},
		schemaDecisionRecorder{ledger.NewService(ledger.NewPostgresRepository(pool))},
		policyConfig)
	for cycle := 1; cycle <= 3; cycle++ {
		_, err := custodian.Scan(ctx)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("cycle %d: Scan error = %v, want the lock_timeout (55P03)", cycle, err)
		}
	}
	if identity == "" || router.callsFor(hot) != 3 {
		t.Fatalf("hot identity %q routed %d times, want 3", identity, router.callsFor(hot))
	}
	if n := schemaGuardRowsFor(t, pool, identity); n != 1 {
		t.Fatalf("blocked identity has %d ledger rows after 3 cycles, want 1", n)
	}
	for _, table := range []string{"b_cold", "c_cold"} {
		requireFKIndexBuilt(t, pool, schemaName, table)
		if calls := router.callsFor(schemaName + "." + table); calls != 1 {
			t.Fatalf("%s routed %d times, want once", table, calls)
		}
	}
}

func requireFKIndexBuilt(t *testing.T, pool *pgxpool.Pool, schemaName, table string) {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND NOT i.indisprimary AND i.indisvalid`,
		schemaName, table).Scan(&count)
	if err != nil {
		t.Fatalf("read %s indexes: %v", table, err)
	}
	if count != 1 {
		t.Fatalf("%s.%s has %d valid FK indexes, want 1", schemaName, table, count)
	}
}
