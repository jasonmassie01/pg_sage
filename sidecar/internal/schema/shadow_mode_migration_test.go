package schema

import (
	"context"
	"strings"
	"testing"
)

// Roadmap 1.4 (shadow mode). Monitored database: sage.shadow_decision,
// one row per decision pg_sage would have taken below a class's earned
// level, at most one pending per (database, fingerprint). Control
// database: sage.trust_shadow_evidence, the scored shadow decisions the
// ledger counts (one per shadow decision), and the reconciler's shadow
// cursor in sage.trust_ledger_state. Additive and idempotent.

const shadowDeployment = "73737373-7373-4373-8373-737373737373"

func shadowCleanup(t *testing.T) {
	t.Helper()
	pool, _ := requireDB(t)
	clean := func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM sage.shadow_decision WHERE fingerprint LIKE 'mig-%'")
		_, _ = pool.Exec(ctx, "DELETE FROM sage.trust_shadow_evidence WHERE deployment_id = $1",
			shadowDeployment)
	}
	clean()
	t.Cleanup(clean)
}

func TestShadowModeMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 3; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"shadow_decision", "trust_shadow_evidence"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'sage' AND table_name = $1`, table).Scan(&n); err != nil ||
			n != 1 {
			t.Fatalf("table %s: %d (%v)", table, n, err)
		}
	}
	var cursor int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'trust_ledger_state'
		  AND column_name = 'shadow_cursor'`).Scan(&cursor); err != nil || cursor != 1 {
		t.Fatalf("shadow_cursor column: %d (%v)", cursor, err)
	}
	for _, index := range []string{"shadow_decision_one_pending",
		"shadow_decision_fingerprint_recent", "shadow_decision_scored",
		"shadow_decision_class_recent", "trust_shadow_evidence_pair"} {
		var valid bool
		if err := pool.QueryRow(ctx, `SELECT i.indisvalid FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'sage' AND c.relname = $1`, index).Scan(&valid); err != nil ||
			!valid {
			t.Fatalf("index %s: valid=%v (%v)", index, valid, err)
		}
	}
}

const insertShadowRow = `INSERT INTO sage.shadow_decision (database_id, fingerprint,
	family, action_class, title, sql, shape, prediction, gate_verdict, gate_reason,
	trusted_verdict, trusted_reason, granted_level, status, score, score_source)
	VALUES ($1, $2, 'tuning', 'index_create', 't', 'CREATE INDEX i ON public.o (a)',
	        'create index on public.o using btree (a)', '{}', $3, 'autonomy_level',
	        'execute', 'autonomy_l3', 1, $4, $5, $6)`

func TestShadowDecisionOnePendingPerFingerprint(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	shadowCleanup(t)
	ins := func(db any, fp, status string, score, source any) error {
		_, err := pool.Exec(ctx, insertShadowRow, db, fp, "observe_only", status, score,
			source)
		return err
	}
	if err := ins(1, "mig-a", "pending", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ins(1, "mig-a", "pending", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("a second pending decision: %v", err)
	}
	// Another database, and scored history, are not duplicates.
	for _, err := range []error{
		ins(2, "mig-a", "pending", nil, nil), ins(nil, "mig-a", "pending", nil, nil),
		ins(1, "mig-a", "scored", "correct", "hypopg"),
		ins(1, "mig-a", "scored", "unscored", "none"),
	} {
		if err != nil {
			t.Fatalf("a legitimate row was refused: %v", err)
		}
	}
	if err := ins(nil, "mig-a", "pending", nil, nil); err == nil {
		t.Fatal("two pending decisions without a database id")
	}
}

func TestShadowDecisionChecks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	shadowCleanup(t)
	for name, args := range map[string][]any{
		"verdict":              {1, "mig-b", "execute", "pending", nil, nil},
		"status":               {1, "mig-c", "observe_only", "done", nil, nil},
		"score":                {1, "mig-d", "observe_only", "scored", "great", "hypopg"},
		"source":               {1, "mig-e", "observe_only", "scored", "correct", "llm"},
		"scored without score": {1, "mig-f", "observe_only", "scored", nil, nil},
		"pending with score":   {1, "mig-g", "observe_only", "pending", "correct", "hypopg"},
	} {
		if _, err := pool.Exec(ctx, insertShadowRow, args...); err == nil ||
			!strings.Contains(err.Error(), "check constraint") {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestTrustShadowEvidenceOncePerDecision(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	shadowCleanup(t)
	insert := `INSERT INTO sage.trust_shadow_evidence (deployment_id, database_name,
		shadow_id, fingerprint, family, action_class, score, source, observed_at)
		VALUES ($1, 'orders', 41, 'mig-x', 'tuning', 'index_create', $2, $3, now())`
	if _, err := pool.Exec(ctx, insert, shadowDeployment, "correct", "hypopg"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, shadowDeployment, "correct", "hypopg"); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("the same shadow decision twice: %v", err)
	}
	bad := `INSERT INTO sage.trust_shadow_evidence (deployment_id, database_name, shadow_id,
		fingerprint, family, action_class, score, source, observed_at)
		VALUES ($1, 'orders', $2, 'mig-y', 'tuning', 'index_create', $3, $4, now())`
	for i, args := range [][]any{{"unscored", "hypopg"}, {"correct", "operator"},
		{"correct", "applied"}} {
		if _, err := pool.Exec(ctx, bad, shadowDeployment, 100+i, args[0], args[1]); err == nil ||
			!strings.Contains(err.Error(), "check constraint") {
			t.Errorf("uncounted evidence %v accepted: %v", args, err)
		}
	}
}
