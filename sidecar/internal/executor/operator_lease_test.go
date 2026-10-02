package executor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Operator typed-target lease (debt item 2): an operator-approved action
// leases the exact objects its SQL changes, so a concurrent custodian or a
// second operator action on the same object is refused (serialize_mode
// park) or serialized (queue) instead of racing it.

func TestOperatorLeaseTargetsFromSQL(t *testing.T) {
	tests := []struct {
		sql  string
		want []string
	}{
		{"CREATE INDEX CONCURRENTLY t_a ON public.t (a)", []string{"public.t"}},
		{"CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS t_a ON ONLY public.t USING btree (a)",
			[]string{"public.t"}},
		{"DROP INDEX CONCURRENTLY IF EXISTS public.t_a", []string{"public.t_a"}},
		{"REINDEX INDEX CONCURRENTLY public.t_a", []string{"public.t_a"}},
		{"REINDEX TABLE public.t", []string{"public.t"}},
		{`VACUUM (FREEZE, ANALYZE) "public"."Orders"`, []string{`"public"."Orders"`}},
		{"ANALYZE public.t", []string{"public.t"}},
		{"ALTER TABLE public.t SET (fillfactor = 90)", []string{"public.t"}},
		{"ALTER TABLE IF EXISTS ONLY public.t SET (fillfactor = 90)", []string{"public.t"}},
		{"ALTER TABLE t RESET (autovacuum_vacuum_scale_factor)", []string{"t"}},
		{"ALTER SYSTEM SET work_mem = '64MB'", nil},
		{"SELECT pg_cancel_backend(42)", nil},
		{"REINDEX DATABASE app", nil},
		{"", nil},
	}
	for _, tt := range tests {
		if got := operatorLeaseTargets(tt.sql); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("operatorLeaseTargets(%q) = %q, want %q", tt.sql, got, tt.want)
		}
	}
}

// REINDEX ... CONCURRENTLY used to name the keyword CONCURRENTLY as its
// object, so the protected-schema check never saw the real index.
func TestValidateRejectsConcurrentReindexOfProtectedSchema(t *testing.T) {
	err := ValidateExecutorSQL("REINDEX INDEX CONCURRENTLY sage.idx_action_log_time")
	if !errors.Is(err, ErrDisallowedSQL) {
		t.Fatalf("concurrent reindex of a sage index = %v, want ErrDisallowedSQL", err)
	}
}

func TestOperatorRequestCarriesTargetObjects(t *testing.T) {
	request, _ := operatorRequest("ALTER TABLE public.t SET (fillfactor = 90)", 7, nil)
	if !reflect.DeepEqual(request.TargetObjs, []string{"public.t"}) {
		t.Fatalf("operator request targets = %q, want [public.t]", request.TargetObjs)
	}
}

func withSerializeGate(e *Executor, mode string) *Executor {
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.SerializeMode = mode
	e.EnableStandingPolicyDocument(doc, nil)
	return e
}

func holderDecision(t *testing.T, pool *pgxpool.Pool, target string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.decision
		(feature, intent, target_objects, policy_version, verdict, risk_tier, reason,
		 evidence_id)
		VALUES ('freeze', 'holder', jsonb_build_array($1::text), 1, 'execute', 'safe',
		        'authorized', $2) RETURNING id`, target,
		fmt.Sprintf("holder-%s-%d", t.Name(), time.Now().UnixNano())).Scan(&id)
	if err != nil {
		t.Fatalf("insert holder decision: %v", err)
	}
	return id
}

// holdTypedLease takes a typed lease on target as actor, like a running
// custodian, and returns its release.
func holdTypedLease(t *testing.T, pool *pgxpool.Pool, actor, target string) func() {
	t.Helper()
	ctx := context.Background()
	targets, err := policy.ResolveTypedTargets(ctx, pool, []string{target})
	if err != nil {
		t.Fatalf("resolve %s: %v", target, err)
	}
	manager := policy.NewPostgresLeaseManager(pool, nil, holderDecision(t, pool, target),
		time.Minute)
	leaseID, err := manager.AcquireTyped(ctx, actor, targets, "holder of "+target)
	if err != nil {
		t.Fatalf("hold lease on %s: %v", target, err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = manager.ReleaseLease(ctx, leaseID) }) }
	t.Cleanup(release)
	return release
}

func operatorLeaseRefusals(t *testing.T, pool *pgxpool.Pool, target string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.decision
		WHERE verdict='blocked' AND reason='ddl_conflict'
		  AND target_objects ? $1`, target).Scan(&count); err != nil {
		t.Fatalf("count operator lease refusals: %v", err)
	}
	return count
}

func findingActions(t *testing.T, pool *pgxpool.Pool, findingID int) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.action_log WHERE finding_id=$1`, findingID).
		Scan(&count); err != nil {
		t.Fatalf("count actions: %v", err)
	}
	return count
}

func TestOperatorActionRefusedWhileCustodianHoldsTarget(t *testing.T) {
	sql := "ALTER TABLE public.{table} SET (autovacuum_vacuum_scale_factor = 0.03)"
	pool, table, findingID := manualFixture(t, sql)
	sql = strings.ReplaceAll(sql, "{table}", table)
	exec := withSerializeGate(manualExecutor(pool), policy.SerializePark)
	release := holdTypedLease(t, pool, "custodian", "public."+table)
	userID := 9

	_, err := exec.ExecuteManual(context.Background(), findingID, sql, "", &userID)

	if !errors.Is(err, ErrTargetLeased) || !errors.Is(err, policy.ErrLeaseConflict) {
		t.Fatalf("operator action on a leased table = %v, want ErrTargetLeased", err)
	}
	if !strings.Contains(err.Error(), "custodian") {
		t.Fatalf("refusal %q does not name the lease holder", err)
	}
	assertProbeUntouched(t, pool, table)
	if n := findingActions(t, pool, findingID); n != 0 {
		t.Fatalf("refused operator action wrote %d action_log rows, want 0", n)
	}
	if n := operatorLeaseRefusals(t, pool, "public."+table); n != 1 {
		t.Fatalf("blocked ddl_conflict decisions = %d, want 1", n)
	}

	release()
	if _, err := exec.ExecuteManual(context.Background(), findingID, sql, "",
		&userID); err != nil {
		t.Fatalf("operator action after the custodian finished = %v, want success", err)
	}
}

// startBlockedOperator runs an operator action whose ALTER waits on an
// ACCESS EXCLUSIVE lock, so it holds its typed lease until unblock.
func startBlockedOperator(t *testing.T, exec *Executor, pool *pgxpool.Pool, table string,
	findingID int, sql string,
) (unblock func(), done <-chan error) {
	t.Helper()
	ctx := context.Background()
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.Exec(ctx, "LOCK TABLE public."+table+
		" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := exec.ExecuteManual(ctx, findingID, sql, "", nil)
		result <- err
	}()
	waitForOperatorLease(t, pool, "public."+table)
	return func() { _ = blocker.Commit(ctx) }, result
}

func waitForOperatorLease(t *testing.T, pool *pgxpool.Pool, target string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var held bool
		if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1
			FROM sage.change_lease WHERE state='active' AND object_name=$1
			  AND actor LIKE 'operator%')`, target).Scan(&held); err != nil {
			t.Fatalf("read lease: %v", err)
		}
		if held {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("operator never took its typed lease on %s", target)
}

func secondOperatorFinding(t *testing.T, pool *pgxpool.Pool, table, sql string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql)
		VALUES ('manual_safety', 'warning', 'table', $1, 'second operator', '{}',
		        'rec', $2) RETURNING id`, "public."+table, sql).Scan(&id); err != nil {
		t.Fatalf("insert second finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE finding_id=$1",
			id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.findings WHERE id=$1", id)
	})
	return id
}

func TestSecondOperatorRefusedInParkMode(t *testing.T) {
	sql := "ALTER TABLE public.{table} SET (autovacuum_vacuum_scale_factor = 0.03)"
	pool, table, findingID := manualFixture(t, sql)
	sql = strings.ReplaceAll(sql, "{table}", table)
	exec := withSerializeGate(manualExecutor(pool), policy.SerializePark)
	secondSQL := "ALTER TABLE public." + table + " SET (autovacuum_vacuum_scale_factor = 0.04)"
	second := secondOperatorFinding(t, pool, table, secondSQL)
	unblock, firstDone := startBlockedOperator(t, exec, pool, table, findingID, sql)

	_, err := exec.ExecuteManual(context.Background(), second, secondSQL, "", nil)
	unblock()

	if !errors.Is(err, ErrTargetLeased) || !strings.Contains(err.Error(), "operator") {
		t.Fatalf("second operator action = %v, want ErrTargetLeased naming the operator", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first operator action = %v, want success", err)
	}
	if n := findingActions(t, pool, second); n != 0 {
		t.Fatalf("refused second action wrote %d rows, want 0", n)
	}
}

func TestSecondOperatorQueuedRunsAfterFirst(t *testing.T) {
	sql := "ALTER TABLE public.{table} SET (autovacuum_vacuum_scale_factor = 0.03)"
	pool, table, findingID := manualFixture(t, sql)
	sql = strings.ReplaceAll(sql, "{table}", table)
	exec := withSerializeGate(manualExecutor(pool), policy.SerializeQueue)
	queue := policy.DefaultLeaseQueueConfig()
	queue.PollInterval, queue.OperatorMaxWait = 20*time.Millisecond, 20*time.Second
	exec.WithLeaseQueue(queue)
	secondSQL := "ALTER TABLE public." + table + " SET (autovacuum_vacuum_scale_factor = 0.04)"
	second := secondOperatorFinding(t, pool, table, secondSQL)
	unblock, firstDone := startBlockedOperator(t, exec, pool, table, findingID, sql)

	secondDone := make(chan error, 1)
	go func() {
		_, err := exec.ExecuteManual(context.Background(), second, secondSQL, "", nil)
		secondDone <- err
	}()
	waitForQueuedEntry(t, pool, secondSQL)
	unblock()

	if err := <-firstDone; err != nil {
		t.Fatalf("first operator action = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("queued second operator action = %v, want it to run after the first", err)
	}
	var options []string
	if err := pool.QueryRow(context.Background(), `SELECT reloptions FROM pg_class
		WHERE oid = to_regclass($1)`, "public."+table).Scan(&options); err != nil {
		t.Fatalf("reloptions: %v", err)
	}
	if !reflect.DeepEqual(options, []string{"autovacuum_vacuum_scale_factor=0.04"}) {
		t.Fatalf("reloptions = %v, want the queued second action applied last", options)
	}
	if n := operatorLeaseRefusals(t, pool, "public."+table); n != 0 {
		t.Fatalf("queued operator action was refused %d times, want 0", n)
	}
}

func waitForQueuedEntry(t *testing.T, pool *pgxpool.Pool, intent string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var queued bool
		if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1
			FROM sage.lease_queue WHERE intent=$1 AND state='waiting')`, intent).
			Scan(&queued); err != nil {
			t.Fatalf("read lease queue: %v", err)
		}
		if queued {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no waiting lease queue entry for %q", intent)
}

func TestOperatorQueueTimeoutRefuses(t *testing.T) {
	sql := "ALTER TABLE public.{table} SET (autovacuum_vacuum_scale_factor = 0.03)"
	pool, table, findingID := manualFixture(t, sql)
	sql = strings.ReplaceAll(sql, "{table}", table)
	exec := withSerializeGate(manualExecutor(pool), policy.SerializeQueue)
	queue := policy.DefaultLeaseQueueConfig()
	queue.PollInterval, queue.OperatorMaxWait = 20*time.Millisecond, 300*time.Millisecond
	exec.WithLeaseQueue(queue)
	holdTypedLease(t, pool, "custodian", "public."+table)
	started := time.Now()

	_, err := exec.ExecuteManual(context.Background(), findingID, sql, "", nil)

	elapsed := time.Since(started)
	if !errors.Is(err, ErrTargetLeased) || !errors.Is(err, policy.ErrLeaseQueueTimeout) {
		t.Fatalf("operator queue timeout = %v, want ErrTargetLeased/ErrLeaseQueueTimeout", err)
	}
	if elapsed < 250*time.Millisecond || elapsed > 10*time.Second {
		t.Fatalf("operator waited %s, want about the 300ms operator queue bound", elapsed)
	}
	assertProbeUntouched(t, pool, table)
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM sage.lease_queue
		WHERE intent=$1 ORDER BY id DESC LIMIT 1`, sql).Scan(&state); err != nil {
		t.Fatalf("read queue entry: %v", err)
	}
	if state != "timed_out" {
		t.Fatalf("queue entry state = %q, want timed_out", state)
	}
	if n := operatorLeaseRefusals(t, pool, "public."+table); n != 1 {
		t.Fatalf("blocked ddl_conflict decisions = %d, want 1", n)
	}
}

// Many operators act on one table at once (park mode): exactly one runs;
// every other is refused with the lease holder named, never half applied.
func TestConcurrentOperatorsOnOneTableAreExclusive(t *testing.T) {
	sql := "ALTER TABLE public.{table} SET (autovacuum_vacuum_scale_factor = 0.03)"
	pool, table, findingID := manualFixture(t, sql)
	sql = strings.ReplaceAll(sql, "{table}", table)
	exec := withSerializeGate(manualExecutor(pool), policy.SerializePark)
	unblock, firstDone := startBlockedOperator(t, exec, pool, table, findingID, sql)
	const others = 4
	results := make(chan error, others)
	for i := 0; i < others; i++ {
		go func() {
			_, err := exec.ExecuteManual(context.Background(), findingID, sql, "", nil)
			results <- err
		}()
	}
	refused := 0
	for i := 0; i < others; i++ {
		if err := <-results; errors.Is(err, ErrTargetLeased) {
			refused++
		} else {
			t.Errorf("concurrent operator action = %v, want ErrTargetLeased", err)
		}
	}
	unblock()

	if err := <-firstDone; err != nil {
		t.Fatalf("lease holder = %v, want success", err)
	}
	if refused != others {
		t.Fatalf("refused = %d, want %d", refused, others)
	}
	if n := findingActions(t, pool, findingID); n != 1 {
		t.Fatalf("action_log rows = %d, want exactly the holder's", n)
	}
}

func TestManualExecuteStatusForLeasedTarget(t *testing.T) {
	err := fmt.Errorf("%w: held by custodian", ErrTargetLeased)
	if status := ManualExecuteStatus(err); status != 409 {
		t.Fatalf("status for a leased target = %d, want 409", status)
	}
	for err, want := range map[error]int{
		ErrFindingNotActionable:          404,
		ErrFindingSQLMismatch:            400,
		errors.New("connection refused"): 500,
	} {
		if status := ManualExecuteStatus(err); status != want {
			t.Errorf("ManualExecuteStatus(%v) = %d, want %d", err, status, want)
		}
	}
}

// Writers that take a typed lease: every DDL mutation, and VACUUM (a
// custodian freeze holds the table for minutes). ANALYZE and settings do not.
func TestIsLeasedMutation(t *testing.T) {
	for sql, want := range map[string]bool{
		"CREATE INDEX CONCURRENTLY t_a ON public.t (a)":    true,
		"DROP INDEX CONCURRENTLY public.t_a":               true,
		"REINDEX INDEX CONCURRENTLY public.t_a":            true,
		"ALTER TABLE public.t SET (fillfactor = 90)":       true,
		"VACUUM (FREEZE) public.t":                         true,
		"vacuum public.t":                                  true,
		"ANALYZE public.t":                                 false,
		"ALTER SYSTEM SET max_slot_wal_keep_size = '10GB'": false,
		"SELECT pg_terminate_backend(42)":                  false,
		"":                                                 false,
	} {
		if got := isLeasedMutation(sql); got != want {
			t.Errorf("isLeasedMutation(%q) = %v, want %v", sql, got, want)
		}
	}
}

func TestLeaseSpecForFindingsAndCustodians(t *testing.T) {
	finding := parkFinding()
	spec, ok := leaseSpecFor(ActionIntent{Lease: &finding})
	if !ok || spec.Actor != "executor" || spec.Kind != "finding" ||
		!reflect.DeepEqual(spec.Targets, []string{"public." + parkTable}) ||
		spec.Intent != finding.RecommendedSQL || spec.Operator {
		t.Fatalf("finding lease spec = %+v ok=%v", spec, ok)
	}
	custodian := custodianFinding(CustodianProposal{Feature: "freeze",
		SQL: "VACUUM (FREEZE) public.t", TargetObjects: []string{"public.t"}})
	spec, ok = leaseSpecFor(ActionIntent{Lease: &custodian})
	if !ok || spec.Actor != "custodian" || spec.Kind != "custodian" {
		t.Fatalf("custodian lease spec = %+v ok=%v, want a custodian lease", spec, ok)
	}
	analyze := analyzerFindingWithSQL("ANALYZE public.t")
	if _, ok := leaseSpecFor(ActionIntent{Lease: &analyze}); ok {
		t.Fatal("ANALYZE took a change lease")
	}
	operator := &TargetLease{Kind: "operator", Actor: "operator", Targets: []string{"public.t"},
		Intent: "x", Operator: true}
	if spec, ok := leaseSpecFor(ActionIntent{TargetLease: operator}); !ok ||
		!reflect.DeepEqual(spec, *operator) {
		t.Fatalf("explicit target lease = %+v ok=%v, want it unchanged", spec, ok)
	}
	if _, ok := leaseSpecFor(ActionIntent{}); ok {
		t.Fatal("an intent without a lease took one")
	}
}

func analyzerFindingWithSQL(sql string) analyzer.Finding {
	return analyzer.Finding{Category: "test", ObjectIdentifier: "public.t", Title: "t",
		RecommendedSQL: sql}
}
