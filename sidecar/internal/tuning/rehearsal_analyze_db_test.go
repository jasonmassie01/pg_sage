package tuning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-04a: a rehearsal measures shared blocks per call on the clone
// (EXPLAIN ANALYZE, BUFFERS) for each target statement the analyze guard
// proves read-only and that has no parameters, and plans the rest. The
// clone is connected only after it is shown not to be production.

// notProduction is a production identity that is not the test server, so
// a second fixture database on it is a separate clone.
func notProduction(context.Context) (clone.ServerIdentity, error) {
	return clone.ServerIdentity{Host: "production.invalid", Port: 5432,
		StartedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), ServerPort: 5432}, nil
}

// requireParser skips where the analyze guard has no parser: without cgo
// no statement can be proven read-only, so none is executed.
func requireParser(t *testing.T) {
	t.Helper()
	if _, err := sqlast.InspectReadQuery("SELECT 1"); errors.Is(err, sqlast.ErrUnavailable) {
		t.Skip("needs the libpg_query parser (cgo build): without it the analyze guard " +
			"proves nothing read-only and no statement is executed")
	}
}

// receiptProvider is a fake DLE-like provider that hands out dsn under a
// fresh clone id each time and names a receipt for it.
type receiptProvider struct {
	dsn           string
	createErr     error
	n             atomic.Int64
	mu            sync.Mutex
	created       []string
	destroyed     []string
	destroyCtxErr []error
	onCreate      func()
}

func (p *receiptProvider) Create(context.Context, clone.CloneSpec) (clone.Clone, error) {
	if p.createErr != nil {
		return clone.Clone{}, p.createErr
	}
	id := fmt.Sprintf("dle-%d", p.n.Add(1))
	p.mu.Lock()
	p.created = append(p.created, id)
	p.mu.Unlock()
	if p.onCreate != nil {
		p.onCreate()
	}
	return clone.Clone{DSN: p.dsn, ID: id, CreatedFrom: time.Now()}, nil
}

func (p *receiptProvider) Destroy(ctx context.Context, c clone.Clone) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed = append(p.destroyed, c.ID)
	p.destroyCtxErr = append(p.destroyCtxErr, ctx.Err())
	return nil
}

func (p *receiptProvider) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }

func (p *receiptProvider) ReceiptOf(id string) (int64, bool) {
	return 4200 + int64(len(id)), id != ""
}

func analyzeRehearser(p clone.Provider, src clone.IdentitySource) Rehearser {
	return NewCloneRehearser(p, RehearsalOptions{MaxCloneAge: time.Hour,
		StatementTimeout: time.Minute, LockTimeout: 5 * time.Second,
		Production: src, Provider: "dle"})
}

func TestRehearsalAnalyze_BlocksPerCallFallForAnUnparameterizedTarget(t *testing.T) {
	requireParser(t)
	p := &receiptProvider{dsn: cloneDB(t)}
	res, err := analyzeRehearser(p, notProduction).Rehearse(context.Background(),
		rehearsalRequest())
	if err != nil {
		t.Fatalf("rehearse: %v", err)
	}
	lit := res.Queries[0]
	if lit.Error != "" || lit.NotExecuted != "" || lit.BeforeBlocks == nil ||
		lit.AfterBlocks == nil {
		t.Fatalf("the literal lookup is executed before and after: %+v", lit)
	}
	if !(*lit.AfterBlocks < *lit.BeforeBlocks/4) {
		t.Fatalf("the index cuts the blocks of the lookup: %d -> %d", *lit.BeforeBlocks,
			*lit.AfterBlocks)
	}
	param := res.Queries[1]
	if param.NotExecuted != notExecutedParams || param.BeforeBlocks != nil ||
		param.AfterBlocks != nil {
		t.Fatalf("a parameterized statement is planned only: %+v", param)
	}
	rec := RehearsalLadder(res, nil, t0)
	if !rec.Passed() || rec.Tier != verify.TierClone ||
		rec.Metric != verify.LadderMetricBlocksPerCall || rec.Predicted == nil ||
		*rec.Predicted >= -75 {
		t.Fatalf("the tier-2 record passes on the blocks: %+v", rec)
	}
	if rec.Evidence["receipt_id"] != res.ReceiptID || res.ReceiptID == 0 ||
		rec.Evidence["provider"] != "dle" || rec.Evidence["executed"] != 1 {
		t.Fatalf("the record names the receipt and provider: %+v (%d)", rec.Evidence,
			res.ReceiptID)
	}
	if len(p.created) != 1 || len(p.destroyed) != 1 {
		t.Fatalf("created %v destroyed %v", p.created, p.destroyed)
	}
}

func TestRehearsalAnalyze_TimeoutRecordsTimedOutNotACost(t *testing.T) {
	requireParser(t)
	p := &receiptProvider{dsn: cloneDB(t)}
	r := NewCloneRehearser(p, RehearsalOptions{MaxCloneAge: time.Hour,
		StatementTimeout: time.Minute, AnalyzeTimeout: 50 * time.Millisecond,
		Production: notProduction})
	req := rehearsalRequest()
	req.Statements = []RehearsalStatement{{QueryID: 9, Text: "SELECT count(*) FROM " +
		"public.orders a CROSS JOIN public.orders b WHERE a.customer_id < 100"}}
	res, err := r.Rehearse(context.Background(), req)
	if err != nil {
		t.Fatalf("a timed-out statement does not fail the rehearsal: %v", err)
	}
	q := res.Queries[0]
	if q.Error != errAnalyzeTimedOut.Error() || q.BeforeBlocks != nil || q.BeforeCost != 0 ||
		q.AfterCost != 0 {
		t.Fatalf("timed out on the clone, no cost: %+v", q)
	}
	rec := RehearsalLadder(res, nil, t0)
	if rec.Result != verify.LadderInconclusive ||
		!strings.Contains(rec.Skipped[0].Reason, "timed out on the clone") {
		t.Fatalf("nothing executed is inconclusive and says why: %+v", rec)
	}
}

// Runs with or without cgo: an unproven statement is never executed.
func TestRehearsalAnalyze_WriteStatementIsNotExecuted(t *testing.T) {
	dsn := cloneDB(t)
	p := &receiptProvider{dsn: dsn}
	req := rehearsalRequest()
	req.Statements = []RehearsalStatement{
		{QueryID: 5, Text: "UPDATE public.orders SET customer_id = 777777 WHERE id = 1"},
		{QueryID: 6, Text: "SELECT pg_terminate_backend(pg_backend_pid()) FROM " +
			"public.orders WHERE customer_id = 3"},
	}
	res, err := analyzeRehearser(p, notProduction).Rehearse(context.Background(), req)
	if err != nil {
		t.Fatalf("rehearse: %v", err)
	}
	for _, q := range res.Queries {
		if q.BeforeBlocks != nil || q.AfterBlocks != nil ||
			(q.Error == "" && !strings.HasPrefix(q.NotExecuted, "not executed: it ")) {
			t.Fatalf("an unproven statement is not executed, the refusal recorded: %+v", q)
		}
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var customer int64
	if err := pool.QueryRow(context.Background(),
		"SELECT customer_id FROM public.orders WHERE id = 1").Scan(&customer); err != nil ||
		customer != 1 {
		t.Fatalf("the UPDATE never ran on the clone: customer_id %d (%v)", customer, err)
	}
}

func TestRehearsal_RefusesProductionBeforeAnyStatement(t *testing.T) {
	prod := dbPool(t)
	dsn := testdb.SkipUnlessLive(t)
	mustExec(t, prod, "DROP TABLE IF EXISTS public.lab04a_guard")
	mustExec(t, prod, "CREATE TABLE public.lab04a_guard (id bigint, k bigint)")
	t.Cleanup(func() { mustExec(t, prod, "DROP TABLE IF EXISTS public.lab04a_guard") })
	alias := strings.Replace(dsn, "127.0.0.1", "localhost", 1)
	if alias == dsn {
		t.Fatal("test DSN has no 127.0.0.1 host to respell")
	}
	req := RehearsalRequest{
		DDL: "CREATE INDEX CONCURRENTLY lab04a_guard_idx ON public.lab04a_guard (k)",
		Statements: []RehearsalStatement{{QueryID: 1,
			Text: "SELECT * FROM public.lab04a_guard WHERE k = 1"}}}
	for name, target := range map[string]string{"production DSN": dsn, "host alias": alias} {
		p := &receiptProvider{dsn: target}
		_, err := analyzeRehearser(p, clone.PoolIdentity(prod)).Rehearse(
			context.Background(), req)
		if !errors.Is(err, clone.ErrTargetIsProduction) {
			t.Fatalf("%s: %v, want ErrTargetIsProduction", name, err)
		}
		if len(p.destroyed) != 1 {
			t.Fatalf("%s: the refused clone is still destroyed: %v", name, p.destroyed)
		}
		rec := RehearsalLadder(RehearsalResult{}, err, t0)
		if rec.Result != verify.LadderSkipped || rec.Passed() {
			t.Fatalf("%s: a refusal is a skipped tier: %+v", name, rec)
		}
	}
	var exists bool
	if err := prod.QueryRow(context.Background(),
		"SELECT to_regclass('public.lab04a_guard_idx') IS NOT NULL").Scan(&exists); err != nil ||
		exists {
		t.Fatalf("the index was built on production: %v (%v)", exists, err)
	}
	assertNoDDLInStatements(t, prod, "lab04a_guard_idx")
}

// assertNoDDLInStatements checks pg_stat_statements, where installed, for
// any statement naming marker.
func assertNoDDLInStatements(t *testing.T, pool *pgxpool.Pool, marker string) {
	t.Helper()
	var installed bool
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass("+
		"'pg_stat_statements') IS NOT NULL").Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Log("pg_stat_statements is not installed on the test server; the index check " +
			"above is the evidence")
		return
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_stat_statements "+
		"WHERE query ILIKE '%' || $1 || '%' AND query NOT ILIKE '%pg_stat_statements%'",
		marker).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("pg_stat_statements shows %d statements naming %s on production", n, marker)
	}
}

func TestRehearsal_CancelledContextStillDestroysTheClone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &receiptProvider{dsn: "postgres://u@clone.invalid:5432/app", onCreate: cancel}
	src := func(ctx context.Context) (clone.ServerIdentity, error) {
		return clone.ServerIdentity{}, ctx.Err()
	}
	_, err := analyzeRehearser(p, src).Rehearse(ctx, rehearsalRequest())
	if !errors.Is(err, context.Canceled) || !errors.Is(err, clone.ErrTargetUnverified) {
		t.Fatalf("a cancelled cycle: %v", err)
	}
	if len(p.destroyed) != 1 || p.destroyCtxErr[0] != nil {
		t.Fatalf("destroyed %v with ctx errors %v: teardown runs on a live context",
			p.destroyed, p.destroyCtxErr)
	}
}

func TestRehearsal_ZeroStatementsIsNotRun(t *testing.T) {
	p := &receiptProvider{dsn: "postgres://unused"}
	req := rehearsalRequest()
	req.Statements = nil
	if _, err := analyzeRehearser(p, notProduction).Rehearse(context.Background(),
		req); !errors.Is(err, ErrNoStatements) {
		t.Fatalf("no statements: %v", err)
	}
	if len(p.created) != 0 {
		t.Fatalf("no clone without statements: %v", p.created)
	}
}

func TestRehearsal_CreateFailureIsASkippedTierWithTheProviderError(t *testing.T) {
	p := &receiptProvider{createErr: errors.New("dle: no free pool")}
	_, err := analyzeRehearser(p, notProduction).Rehearse(context.Background(),
		rehearsalRequest())
	if !errors.Is(err, ErrCloneUnavailable) {
		t.Fatalf("create failure: %v", err)
	}
	rec := RehearsalLadder(RehearsalResult{}, err, t0)
	if rec.Result != verify.LadderSkipped || len(rec.Skipped) != 1 ||
		!strings.Contains(rec.Skipped[0].Reason, "dle: no free pool") {
		t.Fatalf("the skip carries the provider's error: %+v", rec)
	}
}
