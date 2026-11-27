package advisor

import (
	"strings"
	"testing"
)

// advisors-07: a config sub-advisor's row without SQL is kept when it is
// pooler advice or an observation, as an info finding that runs nothing.
// "No change" rows are still dropped.

// No concurrent access tests: parseLLMFindings is a pure function of its
// arguments.

func TestPromptKind_PoolerRowBecomesInfoFinding(t *testing.T) {
	raw := `[{"object_identifier":"instance","severity":"warning","kind":"pooler",` +
		`"rationale":"120 sessions/min open and close: put PgBouncer in front.",` +
		`"recommended_sql":null}]`
	got := parseLLMFindings(raw, "connection_tuning", noopLog)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Severity != "info" || f.RecommendedSQL != "" || f.RollbackSQL != "" ||
		f.ActionRisk != "" {
		t.Errorf("pooler row must be info without SQL, rollback or risk: %+v", f)
	}
	if f.Category != "connection_tuning" || !strings.Contains(f.Recommendation, "PgBouncer") {
		t.Errorf("category %q recommendation %q", f.Category, f.Recommendation)
	}
	if f.ObjectIdentifier != "instance:pooler" {
		t.Errorf("object identifier %q, want instance:pooler (one open row per kind)",
			f.ObjectIdentifier)
	}
}

func TestPromptKind_ObservationRowKept(t *testing.T) {
	raw := `[{"kind":"observation","rationale":"Sessions idle in transaction for 20 min.",` +
		`"recommended_sql":""}]`
	got := parseLLMFindings(raw, "connection_tuning", noopLog)
	if len(got) != 1 || got[0].Severity != "info" || got[0].RecommendedSQL != "" ||
		got[0].ObjectIdentifier != "instance:observation" {
		t.Fatalf("observation row: %+v", got)
	}
}

func TestPromptKind_RowsStillDropped(t *testing.T) {
	cases := map[string]string{
		"no kind":           `[{"rationale":"Use PgBouncer.","recommended_sql":null}]`,
		"setting kind":      `[{"kind":"setting","rationale":"ok","recommended_sql":null}]`,
		"empty rationale":   `[{"kind":"pooler","rationale":"  ","recommended_sql":null}]`,
		"missing rationale": `[{"kind":"pooler","recommended_sql":null}]`,
		"unknown kind":      `[{"kind":"rewrite","rationale":"x","recommended_sql":null}]`,
		"kind not a string": `[{"kind":7,"rationale":"x","recommended_sql":null}]`,
		"apply mechanism": `[{"kind":"pooler","rationale":"x",` +
			`"recommended_sql":"SELECT pg_reload_conf()"}]`,
		"empty array":        `[]`,
		"not json":           `no`,
		"no change, healthy": `[{"rationale":"Healthy, no change.","recommended_sql":""}]`,
	}
	for name, raw := range cases {
		if got := parseLLMFindings(raw, "connection_tuning", noopLog); len(got) != 0 {
			t.Errorf("%s: %d findings, want 0: %+v", name, len(got), got)
		}
	}
}

// A kind does not unlock anything else: a pooler row that carries SQL is
// gated like any other statement, and kind is honoured only in config
// categories (an advisory category keeps its own SQL-less row already).
func TestPromptKind_SQLStillGated(t *testing.T) {
	raw := `[{"kind":"pooler","rationale":"x",` +
		`"recommended_sql":"ALTER SYSTEM SET max_connections = 5"}]`
	if got := parseLLMFindings(raw, "connection_tuning", noopLog); len(got) != 0 {
		t.Errorf("an out-of-range value is dropped whatever the kind: %+v", got)
	}
	raw = `[{"kind":"pooler","rationale":"x","recommended_sql":"CREATE EXTENSION pgbouncer"}]`
	got := parseLLMFindings(raw, "connection_tuning", noopLog)
	if len(got) != 1 || got[0].RecommendedSQL != "" {
		t.Errorf("non-setting SQL in a config category stays advisory: %+v", got)
	}
	raw = `[{"kind":"pooler","rationale":"x","recommended_sql":null}]`
	got = parseLLMFindings(raw, "query_rewrite", noopLog)
	if len(got) != 1 || got[0].ObjectIdentifier != "instance" {
		t.Errorf("advisory category keeps its row as before: %+v", got)
	}
}

func TestPromptKind_SchemaInConnectionPrompt(t *testing.T) {
	if !strings.Contains(connectionSystemPrompt, `"kind":`) {
		t.Error("the connection prompt's element schema must carry kind")
	}
	for _, kind := range []string{"pooler", "observation", "setting"} {
		if !strings.Contains(connectionSystemPrompt, kind) {
			t.Errorf("the connection prompt must name kind %q", kind)
		}
	}
}
