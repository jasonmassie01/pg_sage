package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-05: the first time a red custodian proposal exhausts its failure
// backoff it raises exactly one critical custodian_escalation event and
// records one custodian_backoff decision; later attempts add neither.

func backoffExecutor(t *testing.T) (*Executor, *pgxpool.Pool, *capturingDispatcher) {
	t.Helper()
	pool, _ := requireDB(t)
	exec := New(pool, config.DefaultConfig(), zeroTime(), nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	d := &capturingDispatcher{}
	exec.WithDispatcher(d)
	exec.WithDatabaseName("orders")
	exec.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictBlocked, RiskTier: policy.RiskSafe,
		Reason: "test gate must not be reached"}})
	return exec, pool, d
}

// seedFailures records n failed runs of sql, the newest first.
func seedFailures(t *testing.T, pool *pgxpool.Pool, sql string, n int) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.action_log WHERE sql_executed = $1", sql)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.decision
			WHERE reason = 'custodian_backoff' AND evidence->>'proposed_sql' = $1`, sql)
	})
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log
			(action_type, sql_executed, outcome, executed_at)
			VALUES ('vacuum_table', $1, 'failed', now() - make_interval(mins => $2))`,
			sql, 10+i); err != nil {
			t.Fatalf("seed failure %d: %v", i, err)
		}
	}
}

func uniqueBackoffSQL() (string, string) {
	table := fmt.Sprintf("public.backoff_%d", time.Now().UnixNano())
	return table, "VACUUM " + table
}

func escalations(d *capturingDispatcher) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys []string
	for _, e := range d.events {
		if e.Type == "custodian_escalation" {
			keys = append(keys, e.DedupKey)
		}
	}
	return keys
}

func backoffDecisions(t *testing.T, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.decision
		WHERE reason = 'custodian_backoff' AND verdict = 'blocked'
		  AND evidence->>'proposed_sql' = $1`, sql).Scan(&n); err != nil {
		t.Fatalf("count backoff decisions: %v", err)
	}
	return n
}

// CHECK-03: three failures on a red table produce one escalation; the
// fourth attempt adds none.
func TestCustodianBackoffEscalatesARedTableExactlyOnce(t *testing.T) {
	exec, pool, d := backoffExecutor(t)
	table, sql := uniqueBackoffSQL()
	seedFailures(t, pool, sql, custodianMaxFailures)
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := exec.SubmitCustodianProposalDecision(context.Background(),
			redVacuumProposal(table, sql))
		if !errors.Is(err, ErrCustodianBackoff) ||
			!strings.Contains(err.Error(), "escalate to an operator") {
			t.Fatalf("attempt %d: error %v, want the exhausted backoff", attempt, err)
		}
	}
	keys := escalations(d)
	sum := sha256.Sum256([]byte(sql))
	want := "custodian_backoff:orders:" + hex.EncodeToString(sum[:])
	if len(keys) != 1 || keys[0] != want {
		t.Fatalf("escalation dedup keys = %v, want exactly [%s]", keys, want)
	}
	d.mu.Lock()
	event := d.events[len(d.events)-1]
	d.mu.Unlock()
	if event.Severity != "critical" || event.Data["database"] != "orders" ||
		event.Data["failures"] != custodianMaxFailures ||
		!strings.Contains(event.Subject, table) {
		t.Fatalf("escalation event = %+v", event)
	}
	if n := backoffDecisions(t, pool, sql); n != 1 {
		t.Fatalf("custodian_backoff decisions = %d, want 1", n)
	}
}

// Below the maximum the backoff only delays; nothing escalates.
func TestCustodianBackoffBelowMaximumDoesNotEscalate(t *testing.T) {
	exec, pool, d := backoffExecutor(t)
	table, sql := uniqueBackoffSQL()
	seedFailures(t, pool, sql, custodianMaxFailures-1)
	_, err := exec.SubmitCustodianProposalDecision(context.Background(),
		redVacuumProposal(table, sql))
	// Two failures wait four minutes; the newest is ten minutes old, so the
	// retry is allowed and reaches the (blocking) test gate.
	if errors.Is(err, ErrCustodianBackoff) {
		t.Fatalf("two failures ten minutes ago still back off: %v", err)
	}
	if keys := escalations(d); len(keys) != 0 {
		t.Fatalf("escalations below the maximum = %v", keys)
	}
	if n := backoffDecisions(t, pool, sql); n != 0 {
		t.Fatalf("custodian_backoff decisions below the maximum = %d", n)
	}
}

// A proposal that is not red keeps today's silent backoff.
func TestCustodianBackoffOfANonRedProposalDoesNotEscalate(t *testing.T) {
	exec, pool, d := backoffExecutor(t)
	table, sql := uniqueBackoffSQL()
	seedFailures(t, pool, sql, custodianMaxFailures)
	proposal := redVacuumProposal(table, sql)
	proposal.Feature, proposal.Deadline = "autovacuum_tuning", nil
	_, err := exec.SubmitCustodianProposalDecision(context.Background(), proposal)
	if !errors.Is(err, ErrCustodianBackoff) {
		t.Fatalf("error %v, want the exhausted backoff", err)
	}
	if keys := escalations(d); len(keys) != 0 {
		t.Fatalf("non-red escalations = %v", keys)
	}
}

// Concurrent submitters of the same exhausted proposal escalate once.
func TestCustodianBackoffConcurrentSubmittersEscalateOnce(t *testing.T) {
	exec, pool, d := backoffExecutor(t)
	table, sql := uniqueBackoffSQL()
	seedFailures(t, pool, sql, custodianMaxFailures)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = exec.SubmitCustodianProposalDecision(context.Background(),
				redVacuumProposal(table, sql))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrCustodianBackoff) {
			t.Fatalf("submitter %d: %v, want the exhausted backoff", i, err)
		}
	}
	if keys := escalations(d); len(keys) != 1 {
		t.Fatalf("concurrent escalations = %v, want exactly one", keys)
	}
	if n := backoffDecisions(t, pool, sql); n != 1 {
		t.Fatalf("custodian_backoff decisions = %d, want 1", n)
	}
}

// A new failure streak after an escalation escalates again.
func TestCustodianBackoffNewFailureAfterEscalationEscalatesAgain(t *testing.T) {
	exec, pool, d := backoffExecutor(t)
	table, sql := uniqueBackoffSQL()
	seedFailures(t, pool, sql, custodianMaxFailures)
	ctx := context.Background()
	_, _ = exec.SubmitCustodianProposalDecision(ctx, redVacuumProposal(table, sql))
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome, executed_at) VALUES ('vacuum_table', $1, 'failed', now() + interval '1 second')`,
		sql); err != nil {
		t.Fatalf("seed newer failure: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	_, _ = exec.SubmitCustodianProposalDecision(ctx, redVacuumProposal(table, sql))
	if keys := escalations(d); len(keys) != 2 {
		t.Fatalf("escalations across two streaks = %v, want two", keys)
	}
}

// Without a pool the backoff reads nothing and escalates nothing.
func TestCustodianBackoffWithoutPoolIsInert(t *testing.T) {
	exec := New(nil, config.DefaultConfig(), zeroTime(), nopLog)
	if err := exec.custodianBackoff(context.Background(), "VACUUM public.t"); err != nil {
		t.Fatalf("no-pool backoff = %v, want nil", err)
	}
	exec.escalateCustodianBackoff(context.Background(),
		redVacuumProposal("public.t", "VACUUM public.t"),
		&custodianBackoffError{failures: 3, exhausted: true})
}
