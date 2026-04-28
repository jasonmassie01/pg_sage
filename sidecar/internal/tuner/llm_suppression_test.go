package tuner

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/schema"
)

func TestLLMSuppressionLedgerSurvivesNewTunerInstance(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	_, err := pool.Exec(ctx,
		`DELETE FROM sage.findings
		  WHERE category = 'query_tuning'
		    AND object_identifier LIKE 'llm_suppression:%'`)
	if err != nil {
		t.Fatalf("clean suppression findings: %v", err)
	}
	c := candidate{QueryID: 42, Query: "SELECT * FROM orders"}
	contextKey := llmContextFingerprint(c, `{"Plan":{}}`, []PlanSymptom{{
		Kind: SymptomBadNestedLoop,
	}}, "HashJoin(o c)")
	prescriptionKey := llmPrescriptionFingerprint(c, `{"Plan":{}}`,
		Prescription{HintDirective: "HashJoin(o c)"})
	writer := New(pool, TunerConfig{CascadeCooldownCycles: 2}, nil,
		noopLog2)

	writer.recordLLMSuppression(ctx, c, contextKey, prescriptionKey,
		"accepted", "accepted LLM prescription cooldown")

	reader := New(pool, TunerConfig{CascadeCooldownCycles: 2}, nil,
		noopLog2)
	if !reader.llmSuppressionActive(ctx, contextKey) {
		t.Fatalf("context fingerprint should be suppressed")
	}
	if !reader.llmSuppressionActive(ctx, prescriptionKey) {
		t.Fatalf("prescription fingerprint should be suppressed")
	}
	var status, outcome string
	var suppressedUntil time.Time
	err = pool.QueryRow(ctx,
		`SELECT status, detail->>'llm_outcome', suppressed_until
		   FROM sage.findings
		  WHERE object_identifier = $1`,
		"llm_suppression:"+contextKey,
	).Scan(&status, &outcome, &suppressedUntil)
	if err != nil {
		t.Fatalf("query suppression finding: %v", err)
	}
	if status != llmSuppressionStatus {
		t.Fatalf("status = %q, want %q", status, llmSuppressionStatus)
	}
	if outcome != "accepted" {
		t.Fatalf("llm_outcome = %q, want accepted", outcome)
	}
	if !suppressedUntil.After(time.Now().UTC()) {
		t.Fatalf("suppressed_until = %v, want future time", suppressedUntil)
	}
}

func TestLLMSuppressionLedgerIgnoresExpiredRows(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	_, err := pool.Exec(ctx,
		`DELETE FROM sage.findings
		  WHERE category = 'query_tuning'
		    AND object_identifier LIKE 'llm_suppression:%'`)
	if err != nil {
		t.Fatalf("clean suppression findings: %v", err)
	}
	tu := New(pool, TunerConfig{CascadeCooldownCycles: 1}, nil, noopLog2)
	c := candidate{QueryID: 99, Query: "SELECT * FROM customers"}
	contextKey := llmContextFingerprint(c, "{}", nil, "HashJoin(c o)")
	tu.recordLLMSuppression(ctx, c, contextKey, "",
		"llm_error", "temporary failure")
	_, err = pool.Exec(ctx,
		`UPDATE sage.findings
		    SET suppressed_until = now() - interval '1 second'
		  WHERE object_identifier = $1`,
		"llm_suppression:"+contextKey)
	if err != nil {
		t.Fatalf("expire suppression finding: %v", err)
	}

	if tu.llmSuppressionActive(ctx, contextKey) {
		t.Fatalf("expired suppression should not be active")
	}
}
