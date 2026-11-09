package schema

import (
	"context"
	"strings"
	"testing"
	"time"
)

// policy-02: sage.decision.candidate_key names a candidate across
// verdicts, and idx_decision_candidate_open finds its open rows when an
// execute ends the episode.

func TestDecisionCandidateMigrationIsRegisteredAndBounded(t *testing.T) {
	var last string
	for _, statement := range migrationStatements() {
		if strings.Contains(statement, "idx_decision_candidate_open") {
			last = statement
		}
	}
	if !strings.Contains(last, "candidate_key") ||
		!strings.Contains(last, "idx_decision_candidate_open") {
		t.Fatalf("the last migration is not the candidate-key step:\n%s", last)
	}
	if !strings.Contains(last, "lock_timeout") ||
		!strings.Contains(last, "NOT i.indisvalid") {
		t.Fatalf("the candidate-key step lacks lock_timeout or the INVALID rebuild:\n%s",
			last)
	}
}

func TestDecisionCandidateMigrationAddsColumnAndOpenIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var kind string
	if err := pool.QueryRow(ctx, `SELECT data_type || ':' || is_nullable
		FROM information_schema.columns WHERE table_schema = 'sage'
		  AND table_name = 'decision' AND column_name = 'candidate_key'`).
		Scan(&kind); err != nil {
		t.Fatalf("candidate_key column: %v", err)
	}
	if kind != "text:YES" {
		t.Fatalf("candidate_key = %s, want nullable text", kind)
	}
	def := indexDefinition(t, pool, "idx_decision_candidate_open")
	if !strings.Contains(def, "(candidate_key)") ||
		!strings.Contains(def, "resolved_at IS NULL") ||
		!strings.Contains(def, "candidate_key IS NOT NULL") ||
		strings.Contains(def, "UNIQUE") {
		t.Fatalf("idx_decision_candidate_open = %s, want a partial index on open keys", def)
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index
		WHERE indexrelid = 'sage.idx_decision_candidate_open'::regclass`).
		Scan(&valid); err != nil || !valid {
		t.Fatalf("idx_decision_candidate_open valid = %v (%v), want valid", valid, err)
	}
}

// An INVALID build (a failed build by hand) is rebuilt by the next
// bootstrap, as for the other ledger indexes.
func TestDecisionCandidateIndexInvalidIsRebuilt(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = 'sage.idx_decision_candidate_open'::regclass`); err != nil {
		t.Skipf("cannot mark the index INVALID (needs superuser): %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index
		WHERE indexrelid = 'sage.idx_decision_candidate_open'::regclass`).
		Scan(&valid); err != nil || !valid {
		t.Fatalf("after re-bootstrap valid = %v (%v), want rebuilt", valid, err)
	}
}

// A complete candidate-key step runs again without locking sage.decision.
func TestCompleteCandidateMigrationTakesNoLockOnDecision(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx,
		"LOCK TABLE sage.decision IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock sage.decision: %v", err)
	}
	start := time.Now()
	if _, err := pool.Exec(ctx, ddlDecisionCandidateKey()); err != nil {
		t.Fatalf("re-running the complete candidate-key step waited for a lock: %v", err)
	}
	// A lock wait ends in the step's own 5 s lock_timeout error above.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("re-run took %v, want no lock wait", elapsed)
	}
}
