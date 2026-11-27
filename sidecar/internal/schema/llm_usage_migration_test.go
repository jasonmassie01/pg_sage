package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fleet-10: sage.llm_usage holds today's LLM token usage per database key
// and role in the control database. Bootstrap creates it idempotently and
// its checks refuse malformed rows.
func TestLLMUsageMigration_Table(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	rows, err := pool.Query(ctx, `SELECT column_name || ':' || data_type
		FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'llm_usage'
		ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, col)
	}
	rows.Close()
	// llm-04 (ddlLLMUsageAttribution) appends the attribution columns and
	// moves the key to (day, database_key, role, purpose, model).
	want := "day:date,database_key:text,role:text,tokens:bigint," +
		"updated_at:timestamp with time zone,purpose:text,model:text," +
		"prompt_tokens:bigint,completion_tokens:bigint,reasoning_tokens:bigint," +
		"calls:bigint,failures:bigint,est_cost_usd:numeric,source:text"
	if strings.Join(cols, ",") != want {
		t.Fatalf("columns = %v\nwant %s", cols, want)
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
	requireLLMUsageChecks(t, ctx, pool)
}

func requireLLMUsageChecks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	bad := map[string]string{
		"negative tokens": `('2031-04-01', 'name:a', 'fleet', -1)`,
		"empty key":       `('2031-04-01', '', 'fleet', 1)`,
		"unknown role":    `('2031-04-01', 'name:a', 'admin', 1)`,
	}
	for name, values := range bad {
		_, err := pool.Exec(ctx, `INSERT INTO sage.llm_usage
			(day, database_key, role, tokens) VALUES `+values)
		if err == nil {
			_, _ = pool.Exec(ctx, `DELETE FROM sage.llm_usage WHERE day = '2031-04-01'`)
			t.Fatalf("%s: row accepted, want a check violation", name)
		}
	}
	for _, role := range []string{"general", "optimizer", "shared", "fleet"} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.llm_usage
			(day, database_key, role, tokens) VALUES ('2031-04-01', 'name:a', $1, 0)`,
			role); err != nil {
			t.Fatalf("role %q refused: %v", role, err)
		}
	}
	_, _ = pool.Exec(ctx, `DELETE FROM sage.llm_usage WHERE day = '2031-04-01'`)
}
