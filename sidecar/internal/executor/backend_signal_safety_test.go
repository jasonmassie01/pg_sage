package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Regression tests for G4-B03: backend signals must never run as raw SQL and
// must re-verify pid + backend_start + query identity before signaling.

type sleepingBackend struct {
	pid          int
	backendStart time.Time
	queryStart   time.Time
	queryID      int64
	query        string
	done         chan error
}

func startSleepingBackend(t *testing.T, pool *pgxpool.Pool, seconds int) sleepingBackend {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire sleeper: %v", err)
	}
	backend := sleepingBackend{
		pid:   int(conn.Conn().PgConn().PID()),
		query: fmt.Sprintf("SELECT pg_sleep(%d)", seconds),
		done:  make(chan error, 1),
	}
	go func() {
		_, execErr := conn.Exec(ctx, backend.query)
		conn.Release()
		backend.done <- execErr
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx, `SELECT backend_start, query_start,
			COALESCE(query_id, 0) FROM pg_stat_activity
			WHERE pid=$1 AND state='active' AND query=$2`, backend.pid, backend.query).
			Scan(&backend.backendStart, &backend.queryStart, &backend.queryID)
		if err == nil {
			return backend
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("sleeper %d never became active", backend.pid)
	return backend
}

func (b sleepingBackend) waitUncancelled(t *testing.T) {
	t.Helper()
	select {
	case err := <-b.done:
		if err != nil {
			t.Fatalf("backend %d was signalled: %v", b.pid, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("backend %d did not finish", b.pid)
	}
}

func (b sleepingBackend) waitCancelled(t *testing.T) {
	t.Helper()
	select {
	case err := <-b.done:
		if err == nil || !strings.Contains(err.Error(), "cancel") {
			t.Fatalf("backend %d error = %v, want cancellation", b.pid, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("backend %d was not cancelled", b.pid)
	}
}

func TestExecuteFindingNeverSignalsBackendAsRawSQL(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 2)
	exec := New(pool, config.DefaultConfig(), nil, time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	finding := analyzer.Finding{
		Category: "lock_chain", ObjectIdentifier: fmt.Sprintf("pid:%d", backend.pid),
		Title: "raw cancel", RecommendedSQL: fmt.Sprintf(
			"SELECT pg_cancel_backend(%d);", backend.pid),
	}

	exec.executeFinding(ctx, finding, 0, 0)

	backend.waitUncancelled(t)
}

func TestCustodianBlockerProposalNeverSignals(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 2)
	exec := New(pool, config.DefaultConfig(), nil, time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskModerate,
	}})

	err := exec.SubmitCustodianProposal(ctx, CustodianProposal{
		Feature: "freeze_blocker", TargetObjects: []string{"public.t"},
		SQL:      fmt.Sprintf("SELECT pg_cancel_backend(%d)", backend.pid),
		Evidence: map[string]any{"pid": backend.pid},
	})

	if err == nil {
		t.Fatal("custodian backend signal proposal succeeded, want refusal")
	}
	backend.waitUncancelled(t)
}

func TestBackendEvidenceRequiresBackendStart(t *testing.T) {
	detail, _ := json.Marshal(map[string]any{
		"pid": 42, "query_id": 7, "query_start": time.Now().UTC(),
		"query": "SELECT 1", "app_name": "app",
	})
	_, err := parseBackendEvidence(detail, 42)
	if !errors.Is(err, ErrBackendEvidenceStale) {
		t.Fatalf("parseBackendEvidence without backend_start = %v, want stale", err)
	}
}

func TestSignalMatchingBackendRejectsDifferentBackendStart(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 2)
	exec := New(pool, config.DefaultConfig(), nil, time.Time{}, nopLog)
	evidence := backendSignalEvidence{
		PID: backend.pid, QueryStart: backend.queryStart, Query: backend.query,
		QueryID: backend.queryID, BackendStart: backend.backendStart.Add(-time.Hour),
	}

	err := exec.signalMatchingBackend(ctx, "cancel", evidence)

	if !errors.Is(err, ErrBackendEvidenceStale) {
		t.Fatalf("signal with reused-PID identity = %v, want stale", err)
	}
	backend.waitUncancelled(t)
}

func TestSignalMatchingBackendSignalsExactIdentity(t *testing.T) {
	pool, ctx := requireDB(t)
	backend := startSleepingBackend(t, pool, 10)
	exec := New(pool, config.DefaultConfig(), nil, time.Time{}, nopLog)
	evidence := backendSignalEvidence{
		PID: backend.pid, QueryStart: backend.queryStart, Query: backend.query,
		QueryID: backend.queryID, BackendStart: backend.backendStart,
	}

	if err := exec.signalMatchingBackend(ctx, "cancel", evidence); err != nil {
		t.Fatalf("signalMatchingBackend exact identity: %v", err)
	}
	backend.waitCancelled(t)
}

func TestBackendSignalSQLExcludesProtectedBackends(t *testing.T) {
	for name, query := range map[string]string{
		"cancel": cancelMatchingBackendSQL, "terminate": terminateMatchingBackendSQL,
	} {
		for _, predicate := range []string{
			"a.backend_start = $6", "a.datname = current_database()",
			"a.backend_type = 'client backend'", "pg_dump", "pg_basebackup",
		} {
			if !strings.Contains(query, predicate) {
				t.Fatalf("%s signal SQL lacks %q", name, predicate)
			}
		}
	}
}

func TestRunawayFindingCarriesBackendStart(t *testing.T) {
	start := time.Now().Add(-time.Hour).UTC()
	tracked := &TrackedQuery{
		PID: 9, QueryStart: time.Now().Add(-time.Minute), BackendStart: start,
		QueryText: "SELECT 1", State: "cancelled",
	}

	finding := buildRunawayFinding(tracked, 5)

	got, _ := finding.Detail["backend_start"].(string)
	if got != start.Format(time.RFC3339Nano) {
		t.Fatalf("backend_start detail = %q, want %q", got, start.Format(time.RFC3339Nano))
	}
}
