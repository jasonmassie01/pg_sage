package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The query page's plan is shown to viewers, so its string literals are
// redacted (market-01 review): an auto_explain plan carries the logged
// statement's real parameter values.

func TestRedactPlanJSON_RedactsStringLiteralsKeepsShape(t *testing.T) {
	raw := json.RawMessage(`[{"Plan": {"Node Type": "Index Scan",
		"Index Cond": "(email = 'alice@example.com'::text)",
		"Filter": "(note = $$secret$$)", "Total Cost": 12.345678901234,
		"Plans": [{"Node Type": "Seq Scan", "Filter": "(ssn = '123-45-6789')"}]}}]`)
	got := string(redactPlanJSON(raw))
	for _, leak := range []string{"alice@example.com", "secret", "123-45-6789"} {
		if strings.Contains(got, leak) {
			t.Errorf("redacted plan %s still holds %q", got, leak)
		}
	}
	for _, keep := range []string{`"Index Cond":"(email = '?'::text)"`,
		`"Filter":"(note = $?$)"`, `"Filter":"(ssn = '?')"`, `"Node Type":"Seq Scan"`,
		`"Total Cost":12.345678901234`} {
		if !strings.Contains(got, keep) {
			t.Errorf("redacted plan %s, want it to hold %s", got, keep)
		}
	}
}

func TestRedactPlanJSON_EmptyAndUndecodableAreLeftOut(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, {}, json.RawMessage(`{"Plan": "x'`)} {
		if got := redactPlanJSON(raw); got != nil {
			t.Errorf("redactPlanJSON(%q) = %s, want nil (left out, never unredacted)",
				raw, got)
		}
	}
}

func TestRedactPlanJSON_ScalarsAndNullPassThrough(t *testing.T) {
	for raw, want := range map[string]string{`null`: `null`, `42`: `42`,
		`"plain"`: `"plain"`, `"a 'b' c"`: `"a '?' c"`, `[true]`: `[true]`} {
		if got := string(redactPlanJSON(json.RawMessage(raw))); got != want {
			t.Errorf("redactPlanJSON(%s) = %s, want %s", raw, got, want)
		}
	}
}

// No concurrent access test: redactPlanJSON is pure and shares no state.

func TestQueryDetailDB_PlanLiteralsAreRedacted(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedExplorer(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache (captured_at, queryid,
		plan_json, source, total_cost) VALUES ($1, $2, $3, 'auto_explain', 7)`,
		explorerFrom.Add(45*time.Minute), explorerQID,
		`[{"Plan": {"Node Type": "Index Scan", "Index Cond": "(email = 'bob@x.io'::text)"}}]`,
	); err != nil {
		t.Fatalf("seed a plan with a literal: %v", err)
	}
	code, body := explorerGet(t, pool, "/api/v1/queries/7500000001?database=testdb&"+
		windowQuery())
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	plan, _ := body["plan"].(map[string]any)
	doc, err := json.Marshal(plan["plan_json"])
	if err != nil || plan["total_cost"] != float64(7) {
		t.Fatalf("plan %v (%v), want the newest plan (cost 7)", body["plan"], err)
	}
	if strings.Contains(string(doc), "bob@x.io") ||
		!strings.Contains(string(doc), `(email = '?'::text)`) {
		t.Errorf("plan_json %s, want its literal redacted to '?'", doc)
	}
}
