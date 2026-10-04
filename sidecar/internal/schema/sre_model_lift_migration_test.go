package schema

import (
	"testing"
)

// Roadmap 2.4 (2026-10-04): a stored PGIncidentBench report keeps its
// held-out model lift (sage.sre_eval_runs.model_lift, jsonb, NULL for
// reports without one). Additive and idempotent on top of the bench
// provenance migration; rows stored before it keep working.

func TestSREMigrationModelLift_ColumnIsIdempotentAndNullable(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	provCleanup(t, ctx)
	var dataType, nullable string
	err := pool.QueryRow(ctx, `SELECT data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'sre_eval_runs'
		  AND column_name = 'model_lift'`).Scan(&dataType, &nullable)
	if err != nil || dataType != "jsonb" || nullable != "YES" {
		t.Fatalf("model_lift column = %q nullable %q (%v)", dataType, nullable, err)
	}
	const id = "21212121-2121-4121-8121-212121212121"
	if err := insertRun(ctx, t, id, "bench", "", nil); err != nil {
		t.Fatalf("a report without model lift must insert: %v", err)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT model_lift IS NULL FROM sage.sre_eval_runs
		WHERE deployment_id = $1 AND id = $2`, provDeployment, id).Scan(&isNull); err != nil ||
		!isNull {
		t.Fatalf("model_lift of an old-style row: null %v (%v)", isNull, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_eval_runs
		SET model_lift = '{"llm_mode": "live", "records": []}'
		WHERE deployment_id = $1 AND id = $2`, provDeployment, id); err != nil {
		t.Fatalf("store model lift: %v", err)
	}
	if _, err := pool.Exec(ctx, ddlSREModelLift); err != nil {
		t.Fatalf("re-running the migration: %v", err)
	}
	var mode string
	if err := pool.QueryRow(ctx, `SELECT model_lift->>'llm_mode' FROM sage.sre_eval_runs
		WHERE deployment_id = $1 AND id = $2`, provDeployment, id).Scan(&mode); err != nil ||
		mode != "live" {
		t.Fatalf("model lift lost by a re-run: %q (%v)", mode, err)
	}
}

func TestSREMigrationModelLift_RejectsANonObject(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	provCleanup(t, ctx)
	const id = "31313131-3131-4131-8131-313131313131"
	if err := insertRun(ctx, t, id, "bench", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_eval_runs SET model_lift = '[1, 2]'
		WHERE deployment_id = $1 AND id = $2`, provDeployment, id); err == nil {
		t.Fatal("a model_lift that is not a JSON object was stored")
	}
}
