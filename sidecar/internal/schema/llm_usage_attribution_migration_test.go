package schema

import (
	"testing"
)

// llm-04: ddlLLMUsageAttribution extends fleet-10's sage.llm_usage with the
// attribution columns and keys it by purpose and model; the budget
// counters keep purpose '_total'. Bootstrap runs it twice cleanly.

func TestLLMUsageAttributionMigration_ColumnsAndKey(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var cols string
	if err := pool.QueryRow(ctx, `SELECT string_agg(column_name || ':' || data_type
		|| ':' || coalesce(column_default, ''), ',' ORDER BY column_name)
		FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'llm_usage'
		  AND column_name IN ('purpose', 'model', 'prompt_tokens', 'completion_tokens',
		      'reasoning_tokens', 'calls', 'failures', 'est_cost_usd', 'source')`).
		Scan(&cols); err != nil {
		t.Fatalf("columns: %v", err)
	}
	want := "calls:bigint:0,completion_tokens:bigint:0,est_cost_usd:numeric:," +
		"failures:bigint:0,model:text:''::text,prompt_tokens:bigint:0," +
		"purpose:text:'_total'::text,reasoning_tokens:bigint:0,source:text:'provider'::text"
	if cols != want {
		t.Errorf("attribution columns = %s, want %s", cols, want)
	}
	var pk string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid)
		FROM pg_constraint WHERE conrelid = 'sage.llm_usage'::regclass
		  AND contype = 'p'`).Scan(&pk); err != nil {
		t.Fatalf("primary key: %v", err)
	}
	if pk != "PRIMARY KEY (day, database_key, role, purpose, model)" {
		t.Fatalf("primary key = %q", pk)
	}
}

// Invalid input: the checks refuse a malformed attribution row.
func TestLLMUsageAttributionMigration_Checks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bad := map[string]string{
		"empty purpose":     `('2031-04-02', 'name:a', 'general', '', 'm', 0, 0, 'provider')`,
		"spaced purpose":    `('2031-04-02', 'name:a', 'general', 'a b', 'm', 0, 0, 'provider')`,
		"negative calls":    `('2031-04-02', 'name:a', 'general', 'x', 'm', -1, 0, 'provider')`,
		"negative failures": `('2031-04-02', 'name:a', 'general', 'x', 'm', 0, -1, 'provider')`,
		"unknown source":    `('2031-04-02', 'name:a', 'general', 'x', 'm', 0, 0, 'guess')`,
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.llm_usage WHERE day = '2031-04-02'`)
	}()
	for name, values := range bad {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.llm_usage
			(day, database_key, role, purpose, model, calls, failures, source)
			VALUES `+values); err == nil {
			t.Errorf("%s: insert succeeded, want a check violation", name)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.llm_usage
		(day, database_key, role, purpose, model, calls, est_cost_usd)
		VALUES ('2031-04-02', 'name:a', 'general', 'x', 'm', 1, -0.5)`); err == nil {
		t.Error("negative cost accepted")
	}
	for _, purpose := range []string{"_total", "briefing"} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.llm_usage
			(day, database_key, role, purpose, model, tokens)
			VALUES ('2031-04-02', 'name:a', 'general', $1, '', 5)`, purpose); err != nil {
			t.Errorf("purpose %s beside the other on one key refused: %v", purpose, err)
		}
	}
}
