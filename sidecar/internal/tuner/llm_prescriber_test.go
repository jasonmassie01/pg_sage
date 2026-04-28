package tuner

import (
	"context"
	"testing"
)

func TestParseLLMPrescriptions_Valid(t *testing.T) {
	resp := `[{"hint_directive": "HashJoin(o c)", "rationale": "large unsorted join", "confidence": 0.9}]`
	recs, err := parseLLMPrescriptions(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
	if recs[0].HintDirective != "HashJoin(o c)" {
		t.Errorf("got directive %q", recs[0].HintDirective)
	}
	if recs[0].Confidence != 0.9 {
		t.Errorf("got confidence %f", recs[0].Confidence)
	}
}

func TestParseLLMPrescriptions_Multiple(t *testing.T) {
	resp := `[
		{"hint_directive": "Set(work_mem \"128MB\")", "rationale": "spill", "confidence": 0.8},
		{"hint_directive": "MergeJoin(a b)", "rationale": "sorted", "confidence": 0.7}
	]`
	recs, err := parseLLMPrescriptions(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 recs, got %d", len(recs))
	}
}

func TestParseLLMPrescriptions_Empty(t *testing.T) {
	recs, err := parseLLMPrescriptions("[]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recs != nil {
		t.Errorf("expected nil, got %v", recs)
	}
}

func TestParseLLMPrescriptions_EmptyString(t *testing.T) {
	recs, err := parseLLMPrescriptions("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recs != nil {
		t.Errorf("expected nil, got %v", recs)
	}
}

func TestParseLLMPrescriptions_Malformed(t *testing.T) {
	_, err := parseLLMPrescriptions(`[{"hint_directive": "bad`)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestParseLLMPrescriptions_WithFences(t *testing.T) {
	resp := "```json\n" +
		`[{"hint_directive": "IndexScan(t idx_t_col)", "rationale": "use index", "confidence": 0.85}]` +
		"\n```"
	recs, err := parseLLMPrescriptions(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
}

func TestParseLLMPrescriptions_WithThinking(t *testing.T) {
	resp := "Let me analyze this query...\n\n" +
		`[{"hint_directive": "NestLoop(s)", "rationale": "small inner", "confidence": 0.7}]` +
		"\n\nDone."
	recs, err := parseLLMPrescriptions(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
	if recs[0].HintDirective != "NestLoop(s)" {
		t.Errorf("got %q", recs[0].HintDirective)
	}
}

func TestValidateHintSyntax_ValidDirectives(t *testing.T) {
	cases := []struct {
		name string
		hint string
		want bool
	}{
		{"HashJoin", "HashJoin(o c)", true},
		{"MergeJoin", "MergeJoin(a b)", true},
		{"NestLoop", "NestLoop(s)", true},
		{"IndexScan", "IndexScan(t idx_foo)", true},
		{"IndexOnlyScan", "IndexOnlyScan(t idx_bar)", true},
		{"SeqScan", "SeqScan(t)", true},
		{"NoSeqScan", "NoSeqScan(t)", true},
		{"Parallel", "Parallel(t 4)", true},
		{"NoParallel", "NoParallel(t)", true},
		{"BitmapScan", "BitmapScan(t idx_baz)", true},
		{"Set", `Set(work_mem "256MB")`, true},
		{"Combined", `Set(work_mem "128MB") HashJoin(o c)`, true},
		{"empty", "", false},
		{"spaces", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateHintSyntax(tc.hint)
			if got != tc.want {
				t.Errorf("validateHintSyntax(%q) = %v, want %v",
					tc.hint, got, tc.want)
			}
		})
	}
}

func TestValidateHintSyntax_RejectsSQL(t *testing.T) {
	cases := []string{
		"DROP TABLE users",
		"DELETE FROM hint_plan.hints",
		"INSERT INTO foo VALUES(1)",
		"ALTER TABLE foo ADD COLUMN bar int",
		"CREATE INDEX idx ON foo(bar)",
		"HashJoin(t); DROP TABLE users",
		"HashJoin(t) -- comment",
		"TRUNCATE hint_plan.hints",
		"GRANT ALL ON hint_plan.hints TO public",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			if validateHintSyntax(tc) {
				t.Errorf("expected rejection for %q", tc)
			}
		})
	}
}

func TestSplitHintDirectives(t *testing.T) {
	parts := splitHintDirectives(
		`Set(work_mem "256MB") HashJoin(o c) IndexScan(t idx)`,
	)
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d: %v", len(parts), parts)
	}
	if parts[0] != `Set(work_mem "256MB")` {
		t.Errorf("part 0 = %q", parts[0])
	}
	if parts[1] != " HashJoin(o c)" {
		// Note: leading space is trimmed in validation
		got := parts[1]
		if got != " HashJoin(o c)" {
			t.Errorf("part 1 = %q", got)
		}
	}
}

func TestConvertPrescriptions_FiltersInvalid(t *testing.T) {
	logCalled := false
	logFn := func(comp, msg string, args ...any) {
		logCalled = true
	}
	recs := []LLMPrescription{
		{HintDirective: "HashJoin(o c)", Rationale: "good"},
		{HintDirective: "DROP TABLE foo", Rationale: "bad"},
		{HintDirective: "", Rationale: "empty"},
	}
	out := convertPrescriptions(recs, logFn)
	if len(out) != 1 {
		t.Fatalf("expected 1 valid, got %d", len(out))
	}
	if out[0].HintDirective != "HashJoin(o c)" {
		t.Errorf("got %q", out[0].HintDirective)
	}
	if !logCalled {
		t.Error("expected log call for rejected hints")
	}
}

func TestLLMPrescriptionFingerprintNormalizesWhitespace(t *testing.T) {
	c := candidate{QueryID: 42, Query: "SELECT  *  FROM orders"}
	p := Prescription{HintDirective: "HashJoin(o c)"}

	got := llmPrescriptionFingerprint(c,
		`{"Plan":  {"Node Type":  "Hash Join"}}`, p)
	want := llmPrescriptionFingerprint(
		candidate{QueryID: 42, Query: "select * from orders"},
		`{"plan": {"node type": "hash join"}}`, p)

	if got != want {
		t.Fatalf("fingerprints differ for equivalent text")
	}
}

func TestFilterRepeatedLLMPrescriptionsSuppressesDuplicate(t *testing.T) {
	tu := New(nil, TunerConfig{CascadeCooldownCycles: 2}, nil,
		func(string, string, ...any) {})
	c := candidate{QueryID: 42, Query: "SELECT * FROM orders"}
	rx := []Prescription{{
		Symptom:       SymptomBadNestedLoop,
		HintDirective: "HashJoin(o c)",
		Rationale:     "same diagnosis",
	}}

	first := tu.filterRepeatedLLMPrescriptions(
		context.Background(), c, "{}", "context-key", rx)
	second := tu.filterRepeatedLLMPrescriptions(
		context.Background(), c, "{}", "context-key", rx)

	if len(first) != 1 {
		t.Fatalf("first prescription len = %d, want 1", len(first))
	}
	if len(second) != 0 {
		t.Fatalf("second prescription len = %d, want 0", len(second))
	}
}

func TestFilterRepeatedLLMPrescriptionsAllowsAfterCooldownTicks(t *testing.T) {
	tu := New(nil, TunerConfig{CascadeCooldownCycles: 1}, nil,
		func(string, string, ...any) {})
	c := candidate{QueryID: 42, Query: "SELECT * FROM orders"}
	rx := []Prescription{{HintDirective: "HashJoin(o c)"}}

	_ = tu.filterRepeatedLLMPrescriptions(
		context.Background(), c, "{}", "context-key", rx)
	tu.tickCooldowns()
	got := tu.filterRepeatedLLMPrescriptions(
		context.Background(), c, "{}", "context-key", rx)

	if len(got) != 1 {
		t.Fatalf("prescription len after cooldown = %d, want 1", len(got))
	}
}
