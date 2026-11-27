//go:build cgo

package rca

import (
	"encoding/json"
	"strings"
	"testing"
)

const lockWaitersSQL = "SELECT pid, state FROM pg_stat_activity WHERE wait_event_type = 'Lock'"

// The proof runs on the engine's store inside the bounded read-only
// transaction production uses.
func TestTier2ProofOnRealCatalog(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	cases := map[string]string{
		lockWaitersSQL:                     "",
		"SELECT pg_terminate_backend(123)": "volatile function",
		"SELECT * FROM pg_catalog.pg_class c JOIN information_schema.tables t " +
			"ON t.table_name = c.relname": "",
		"SELECT * FROM sage_no_such_table": "unknown relation",
	}
	for sql, refusal := range cases {
		inc := buildTier2Incident(tier2Response{RootCause: "x",
			RecommendedSQL: []string{sql}}, testSignals(1))
		vetTier2SQL(ctx, pool, &inc, noopLog)
		if refusal == "" {
			if inc.RecommendedSQL != sql || withheldNote(inc) != "" {
				t.Errorf("%s: withheld (%q), want kept", sql, withheldNote(inc))
			}
			continue
		}
		if inc.RecommendedSQL != "" || !strings.Contains(withheldNote(inc), refusal) {
			t.Errorf("%s: SQL %q note %q, want withheld because %q", sql,
				inc.RecommendedSQL, withheldNote(inc), refusal)
		}
	}
}

// CHECK-05: no stored Tier 2 incident carries non-read-only SQL.
func TestTier2StoredIncidentsCarryOnlyProvenSQL(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	for sql, kept := range map[string]bool{
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE state = 'idle'": false,
		lockWaitersSQL: true,
	} {
		eng, db := lifecycleEngine(t, pool)
		resp := validTier2Response()
		resp.RecommendedSQL = []string{sql}
		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		srv := fakeLLMServer(string(raw))
		eng.WithLLM(tier2LLMClient(srv.URL))
		eng.cfg.LLMCorrelationThreshold = 3
		if err := eng.Hydrate(ctx, pool); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		curr, prev, lcf := realTier2Inputs()
		eng.AnalyzeContext(ctx, curr, prev, testConfig(), lcf)
		srv.Close()
		if err := eng.PersistIncidents(ctx, pool); err != nil {
			t.Fatalf("PersistIncidents: %v", err)
		}
		var stored []string
		rows, err := pool.Query(ctx, `SELECT COALESCE(recommended_sql, '')
			FROM sage.incidents WHERE database_name = $1 AND source = 'llm'`, db)
		if err != nil {
			t.Fatalf("query incidents: %v", err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			stored = append(stored, s)
		}
		rows.Close()
		want := ""
		if kept {
			want = sql
		}
		if len(stored) != 1 || stored[0] != want {
			t.Errorf("%s: stored llm recommended_sql = %q, want [%q]", sql, stored, want)
		}
	}
}
