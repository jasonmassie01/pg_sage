package schema

import (
	"context"
	"strings"
	"testing"
	"time"
)

// SRE performance fix (v1.8.3, reviews/2026-10-03-perf/FIX-BRIEF.md): the
// earned-autonomy reconcile, the watch lookup, the autovacuum-cancellation
// probe and the investigation list read their rows through an index, and
// SLI samples carry the running counters that make an SLO window a few
// primary-key probes per series.

// sreperfIndexes are the migration's indexes and the fragments their
// definitions must contain.
var sreperfIndexes = map[string][]string{
	"idx_decision_autonomy_execute": {"ON sage.decision", "(id)",
		"verdict = 'execute'", "evidence ? 'incident_family'"},
	"idx_action_queue_autonomy": {"ON sage.action_queue", "(action_log_id)",
		"identity_key ~~ 'autonomy:%'"},
	"idx_verification_watch": {"ON sage.verification", "baseline ->> 'watch_id'"},
	"idx_incidents_autovacuum_cancel": {"ON sage.incidents", "(detected_at)",
		"'log_autovacuum_cancel'", "signal_ids"},
	"idx_sre_investigations_created": {"ON sage.sre_investigations",
		"(deployment_id, database_id, created_at DESC, id DESC)"},
	"idx_sre_investigations_case": {"ON sage.sre_investigations",
		"(deployment_id, database_id, source_case_id, created_at DESC, id DESC)"},
}

func TestSREPerfMigration_CreatesIndexesAndColumns(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for name, fragments := range sreperfIndexes {
		var def string
		var valid bool
		err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(i.indexrelid), i.indisvalid
			FROM pg_index i WHERE i.indexrelid = to_regclass('sage.' || $1)`,
			name).Scan(&def, &valid)
		if err != nil {
			t.Errorf("index %s missing: %v", name, err)
			continue
		}
		for _, f := range fragments {
			if !strings.Contains(def, f) {
				t.Errorf("index %s = %s, want it to contain %q", name, def, f)
			}
		}
		if !valid {
			t.Errorf("index %s is not valid", name)
		}
	}
	want := map[string]string{"cum_bad": "double precision",
		"cum_eligible": "double precision", "cum_samples": "bigint",
		"cum_resets": "bigint", "chain_start": "timestamp with time zone"}
	for column, typ := range want {
		var got string
		err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod)
			FROM pg_attribute WHERE attrelid = 'sage.sre_sli_samples'::regclass
			  AND attname = $1 AND NOT attisdropped`, column).Scan(&got)
		if err != nil || got != typ {
			t.Errorf("sre_sli_samples.%s = %q (%v), want %s", column, got, err, typ)
		}
	}
}

// Re-running the complete migration (every startup) checks the catalog
// first, so it waits for no lock on the tables it indexes: a held SHARE
// UPDATE EXCLUSIVE lock (autovacuum, a concurrent index build) would
// block a bare CREATE INDEX IF NOT EXISTS or ALTER TABLE.
func TestSREPerfMigration_RerunTakesNoLock(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE sage.decision, sage.action_queue,
		sage.verification, sage.incidents, sage.sre_investigations,
		sage.sre_sli_samples IN SHARE UPDATE EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock tables: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET lock_timeout = '1s'"); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET lock_timeout") }()
	start := time.Now()
	if _, err := conn.Exec(ctx, ddlSREPerf()); err != nil {
		t.Fatalf("re-running the SRE performance migration waited for a lock: %v", err)
	}
	// The lock is held throughout, so a lock wait could only end in the
	// 1 s lock_timeout error above; the duration only guards against a hang
	// and is generous for a loaded runner.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("re-run took %v, want no lock wait", elapsed)
	}
}

// An INVALID index left by a failed build is rebuilt; a valid one is kept
// (its OID survives a re-run).
func TestSREPerfMigration_RebuildsInvalidKeepsValid(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	oidOf := func(name string) uint32 {
		var oid uint32
		if err := pool.QueryRow(ctx, `SELECT COALESCE(to_regclass('sage.' || $1)::oid, 0)`,
			name).Scan(&oid); err != nil {
			t.Fatalf("oid of %s: %v", name, err)
		}
		return oid
	}
	kept := oidOf("idx_incidents_autovacuum_cancel")
	broken := oidOf("idx_verification_watch")
	if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = 'sage.idx_verification_watch'::regclass`); err != nil {
		t.Fatalf("mark invalid: %v", err)
	}
	if _, err := pool.Exec(ctx, ddlSREPerf()); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := oidOf("idx_incidents_autovacuum_cancel"); got != kept || kept == 0 {
		t.Fatalf("valid index rebuilt: oid %d -> %d", kept, got)
	}
	rebuilt := oidOf("idx_verification_watch")
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index
		WHERE indexrelid = 'sage.idx_verification_watch'::regclass`).Scan(&valid); err != nil {
		t.Fatalf("validity: %v", err)
	}
	if rebuilt == broken || !valid {
		t.Fatalf("invalid index not rebuilt: oid %d -> %d, valid %v", broken, rebuilt, valid)
	}
}
