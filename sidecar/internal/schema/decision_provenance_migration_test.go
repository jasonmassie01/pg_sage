package schema

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// policy-03: sage.decision.policy_id referenced sage.policy, which in
// fleet mode lives in the control database, and nothing ever wrote it.
// The migration drops the column (and with it the foreign key and
// idx_decision_policy) and adds policy_doc_sha256, the SHA-256 of the
// policy document the gate evaluated.

func TestDecisionProvenanceMigrationIsRegisteredAfterLedgerAndBounded(t *testing.T) {
	statements := migrationStatements()
	ledger, provenance := -1, -1
	for i, statement := range statements {
		if statement == ddlDecisionLedger() {
			ledger = i
		}
		if strings.Contains(statement, "policy_doc_sha256") &&
			strings.Contains(statement, "policy_id") {
			provenance = i
		}
	}
	// The integration chain appends other items' steps after it, so the
	// step is checked to follow the ledger it alters, not to be last.
	if provenance < 0 || provenance < ledger {
		t.Fatalf("decision provenance step at %d, decision ledger at %d: want it "+
			"registered after the ledger", provenance, ledger)
	}
	if step := statements[provenance]; !strings.Contains(step, "lock_timeout") {
		t.Fatalf("the provenance step has no lock_timeout:\n%s", step)
	}
	for _, statement := range statements {
		if strings.Contains(statement, "idx_decision_policy") {
			t.Fatalf("a migration still builds idx_decision_policy:\n%s", statement)
		}
	}
	if strings.Contains(ddlAgentNativeLedger, "policy_id") {
		t.Fatal("a fresh sage.decision still declares policy_id")
	}
}

func decisionColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var kind string
	err := pool.QueryRow(ctx, `SELECT COALESCE(max(data_type || ':' || is_nullable), '')
		FROM information_schema.columns WHERE table_schema = 'sage'
		  AND table_name = 'decision' AND column_name = $1`, name).Scan(&kind)
	if err != nil {
		t.Fatalf("read column %s: %v", name, err)
	}
	return kind
}

func assertProvenanceSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if kind := decisionColumn(t, ctx, pool, "policy_doc_sha256"); kind != "text:YES" {
		t.Fatalf("policy_doc_sha256 = %q, want nullable text", kind)
	}
	if kind := decisionColumn(t, ctx, pool, "policy_id"); kind != "" {
		t.Fatalf("policy_id still exists (%s)", kind)
	}
	var index *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.idx_decision_policy')::text`).
		Scan(&index); err != nil || index != nil {
		t.Fatalf("idx_decision_policy = %v (%v), want dropped", index, err)
	}
	var policyKeys int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.decision'::regclass AND contype = 'f'
		  AND confrelid = 'sage.policy'::regclass`).Scan(&policyKeys); err != nil ||
		policyKeys != 0 {
		t.Fatalf("sage.decision keeps %d foreign keys into sage.policy (%v), want 0",
			policyKeys, err)
	}
}

func TestDecisionProvenanceMigrationOnAFreshDatabase(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	assertProvenanceSchema(t, ctx, pool)
}

// An install from before policy-03 has policy_id, its foreign key and
// idx_decision_policy, and no hash column. Bootstrap upgrades it, twice.
func TestDecisionProvenanceMigrationUpgradesAnOldLedger(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.decision DROP COLUMN policy_doc_sha256;
		ALTER TABLE sage.decision ADD COLUMN policy_id bigint REFERENCES sage.policy(id);
		CREATE INDEX idx_decision_policy ON sage.decision (policy_id)
		    WHERE policy_id IS NOT NULL`); err != nil {
		t.Fatalf("recreate the pre-policy-03 ledger: %v", err)
	}
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	assertProvenanceSchema(t, ctx, pool)
}

func TestDecisionHashCheckAcceptsOnlyLowerCaseSHA256(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	insert := `INSERT INTO sage.decision (feature, intent, verdict, risk_tier, reason,
		evidence_id, policy_version, policy_doc_sha256)
		VALUES ('index', 'index', 'blocked', 'safe', 'x', $1, 1, $2) RETURNING id`
	good := strings.Repeat("0123456789abcdef", 4)
	for _, hash := range []*string{&good, nil} {
		var id int64
		evidenceID := "p03-hash-" + time.Now().Format(time.RFC3339Nano)
		if err := pool.QueryRow(ctx, insert, evidenceID, hash).Scan(&id); err != nil {
			t.Fatalf("hash %v rejected: %v", hash, err)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM sage.decision WHERE id = $1`, id)
	}
	for _, hash := range []string{"", good[:63], good + "a", strings.ToUpper(good),
		strings.Repeat("z", 64)} {
		_, err := pool.Exec(ctx, insert, "p03-bad-"+time.Now().Format(time.RFC3339Nano), hash)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("hash %q: err = %v, want a CHECK violation", hash, err)
		}
	}
}

// A complete provenance step runs again without locking sage.decision.
func TestCompleteProvenanceMigrationTakesNoLockOnDecision(t *testing.T) {
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
	if _, err := pool.Exec(ctx, ddlDecisionPolicyProvenance); err != nil {
		t.Fatalf("re-running the complete provenance step waited for a lock: %v", err)
	}
}
