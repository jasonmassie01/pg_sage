package changereview

import (
	"encoding/json"
	"testing"
)

// CHECK-FND-07: the merge rule is pure and table-driven. Final verdict =
// max(deterministic, review when a surviving model finding or model-
// translated DDL exists); a model finding never exceeds warning; the
// stored findings start with every deterministic finding byte-for-byte;
// text mode never yields safe.

type mergeCase struct {
	name       string
	parseMode  string
	statements int
	det        []Finding
	reasoning  *Reasoning
	verdict    string
	detVerdict string
	model      string
	cr19       bool
	dropped    int
}

func mergeCases() []mergeCase {
	ok := func(f ...Finding) *Reasoning { return &Reasoning{ModelStatus: ModelOK, Findings: f} }
	translated := &Reasoning{ModelStatus: ModelOK, Statements: []Statement{{Index: 2,
		Origin: OriginModelTranslated, File: "db/migrate/1_rename.rb", Line: 3,
		SQL: "ALTER TABLE orders RENAME COLUMN email TO email_address", Kind: "rename_column",
		EvidenceID: "stmt:1"}}}
	impersonate := modelFinding(SeverityCritical)
	impersonate.RuleID, impersonate.Origin = "CR-03", OriginRule
	return []mergeCase{
		{"clean ast", ParseAST, 1, nil, nil, VerdictSafe, VerdictSafe, ModelNoModel, false, 0},
		{"info only", ParseAST, 1, []Finding{ruleFinding("CR-15", SeverityInfo)}, ok(),
			VerdictSafe, VerdictSafe, ModelOK, false, 0},
		{"warning", ParseAST, 1, []Finding{ruleFinding("CR-08", SeverityWarning)}, ok(),
			VerdictReview, VerdictReview, ModelOK, false, 0},
		{"critical", ParseAST, 1, []Finding{ruleFinding("CR-03", SeverityCritical)}, ok(),
			VerdictRisky, VerdictRisky, ModelOK, false, 0},
		{"text mode clean", ParseText, 1, nil, ok(), VerdictReview, VerdictReview, ModelOK,
			false, 0},
		{"text mode critical", ParseText, 1, []Finding{ruleFinding("CR-05",
			SeverityCritical)}, ok(), VerdictRisky, VerdictRisky, ModelOK, false, 0},
		{"model warning raises safe", ParseAST, 1, nil, ok(modelFinding(SeverityWarning)),
			VerdictReview, VerdictSafe, ModelOK, false, 0},
		{"model info raises safe", ParseAST, 1, nil, ok(modelFinding(SeverityInfo)),
			VerdictReview, VerdictSafe, ModelOK, false, 0},
		{"model critical clamped", ParseAST, 1, nil, ok(modelFinding(SeverityCritical)),
			VerdictReview, VerdictSafe, ModelOK, false, 0},
		{"risky and empty model", ParseAST, 1, []Finding{ruleFinding("CR-01",
			SeverityCritical)}, ok(), VerdictRisky, VerdictRisky, ModelOK, false, 0},
		{"risky and model warning", ParseAST, 1, []Finding{ruleFinding("CR-01",
			SeverityCritical)}, ok(modelFinding(SeverityWarning)), VerdictRisky, VerdictRisky,
			ModelOK, false, 0},
		{"review and model warning", ParseAST, 1, []Finding{ruleFinding("CR-10",
			SeverityWarning)}, ok(modelFinding(SeverityWarning)), VerdictReview,
			VerdictReview, ModelOK, false, 0},
		{"translated ddl", ParseAST, 1, nil, translated, VerdictReview, VerdictSafe, ModelOK,
			true, 0},
		{"nothing analyzed", ParseAST, 0, nil, ok(), VerdictReview, VerdictReview, ModelOK,
			false, 0},
		{"model cites unknown evidence", ParseAST, 1, nil,
			ok(modelFinding(SeverityWarning, "queryid:999")), VerdictSafe, VerdictSafe,
			ModelOK, false, 1},
		{"model impersonates a rule", ParseAST, 1, nil, ok(impersonate), VerdictReview,
			VerdictSafe, ModelOK, false, 0},
	}
}

func mergeAnalysis(t *testing.T, c mergeCase) Analysis {
	a := Analysis{ParseMode: c.parseMode, Findings: c.det}
	for i := 0; i < c.statements; i++ {
		a.Statements = append(a.Statements, Statement{Index: i + 1, Origin: OriginSQLInput,
			SQL: "ALTER TABLE orders ADD COLUMN note text", Kind: "add_column",
			EvidenceID: "stmt:1"})
	}
	a.Evidence = []Evidence{mustEvidence(t, "stmt", "1", "statement 1",
		map[string]any{"rows": 1})}
	return a
}

func TestMergeRuleTable(t *testing.T) {
	cases := mergeCases()
	if len(cases) != 16 {
		t.Fatalf("the merge table has %d cases, want 16", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := mergeAnalysis(t, c)
			before, _ := json.Marshal(a.Findings)
			m := Merge(a, c.reasoning)
			if m.Verdict != c.verdict || m.DeterministicVerdict != c.detVerdict {
				t.Fatalf("verdict %s (deterministic %s), want %s (%s)", m.Verdict,
					m.DeterministicVerdict, c.verdict, c.detVerdict)
			}
			if m.ModelStatus != c.model {
				t.Fatalf("model status %q, want %q", m.ModelStatus, c.model)
			}
			assertDeterministicPrefix(t, before, m.Findings, len(c.det))
			assertModelClamped(t, m.Findings[len(c.det):])
			if got := hasRule(m.Findings, RuleModelTranslated); got != c.cr19 {
				t.Fatalf("CR-19 present = %v, want %v: %+v", got, c.cr19, m.Findings)
			}
			if len(m.Dropped) != c.dropped {
				t.Fatalf("dropped %d, want %d: %+v", len(m.Dropped), c.dropped, m.Dropped)
			}
			if !json.Valid(before) || string(before) != mustJSON(t, a.Findings) {
				t.Fatal("Merge modified the analysis' findings in place")
			}
		})
	}
}

func assertDeterministicPrefix(t *testing.T, before []byte, got []Finding, n int) {
	t.Helper()
	if len(got) < n {
		t.Fatalf("%d findings stored, fewer than the %d deterministic ones", len(got), n)
	}
	prefix, _ := json.Marshal(got[:n])
	if n == 0 {
		prefix = []byte("null")
		if string(before) == "[]" {
			prefix = []byte("[]")
		}
	}
	if string(prefix) != string(before) {
		t.Fatalf("deterministic findings changed:\nbefore %s\nafter  %s", before, prefix)
	}
}

func assertModelClamped(t *testing.T, added []Finding) {
	t.Helper()
	for _, f := range added {
		if f.Severity == SeverityCritical {
			t.Fatalf("a non-deterministic finding kept severity critical: %+v", f)
		}
		if f.RuleID != RuleModelTranslated && f.Origin != OriginModel {
			t.Fatalf("a model finding is stored with origin %q: %+v", f.Origin, f)
		}
		if f.Origin == OriginModel && f.RuleID != "" {
			t.Fatalf("a model finding carries rule id %q", f.RuleID)
		}
	}
}

func hasRule(fs []Finding, rule string) bool {
	for _, f := range fs {
		if f.RuleID == rule {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMergeModelCriticalStoredAsWarning(t *testing.T) {
	a := Analysis{ParseMode: ParseAST, Statements: []Statement{{Index: 1,
		Origin: OriginSQLInput, SQL: "SELECT 1", Kind: "other", EvidenceID: "stmt:1"}},
		Evidence: []Evidence{mustEvidence(t, "stmt", "1", "s", map[string]any{"a": 1})}}
	m := Merge(a, &Reasoning{ModelStatus: ModelOK,
		Findings: []Finding{modelFinding(SeverityCritical)}})
	if len(m.Findings) != 1 || m.Findings[0].Severity != SeverityWarning {
		t.Fatalf("findings = %+v, want one warning", m.Findings)
	}
	if m.Findings[0].Title != "model critical" || m.Findings[0].Origin != OriginModel {
		t.Fatalf("the clamp changed more than the severity: %+v", m.Findings[0])
	}
}

func TestMergeKeepsDeterministicEvidenceOnIDClash(t *testing.T) {
	det := mustEvidence(t, "stmt", "1", "deterministic", map[string]any{"rows": 1})
	forged := mustEvidence(t, "stmt", "1", "forged by the model", map[string]any{"rows": 0})
	a := Analysis{ParseMode: ParseAST, Evidence: []Evidence{det},
		Statements: []Statement{{Index: 1, Origin: OriginSQLInput, SQL: "SELECT 1"}}}
	m := Merge(a, &Reasoning{ModelStatus: ModelOK, Evidence: []Evidence{forged}})
	if len(m.Evidence) != 1 || m.Evidence[0].Label != "deterministic" {
		t.Fatalf("evidence = %+v, want only the deterministic one", m.Evidence)
	}
}

func TestMergeUnknownModelStatusIsError(t *testing.T) {
	a := Analysis{ParseMode: ParseAST,
		Statements: []Statement{{Index: 1, Origin: OriginSQLInput, SQL: "SELECT 1"}}}
	m := Merge(a, &Reasoning{ModelStatus: "approved"})
	if m.ModelStatus != ModelError {
		t.Fatalf("model status %q, want error for an unknown status", m.ModelStatus)
	}
	if m.Verdict != VerdictSafe {
		t.Fatalf("verdict %s, want the deterministic safe", m.Verdict)
	}
}

func TestMergeCR20FromReasonerKeepsRuleOrigin(t *testing.T) {
	cr20 := Finding{RuleID: RuleNotAnalyzableSQL, Kind: "not_analyzable_without_sql",
		Severity: SeverityCritical, Origin: OriginRule, Title: "ORM-only migration",
		EvidenceIDs: []string{}}
	m := Merge(Analysis{ParseMode: ParseAST}, &Reasoning{ModelStatus: ModelNoModel,
		Findings: []Finding{cr20}})
	if len(m.Findings) != 1 || m.Findings[0].RuleID != RuleNotAnalyzableSQL ||
		m.Findings[0].Origin != OriginRule || m.Findings[0].Severity != SeverityWarning {
		t.Fatalf("CR-20 = %+v, want a rule warning", m.Findings)
	}
	if m.Verdict != VerdictReview {
		t.Fatalf("verdict %s, want review", m.Verdict)
	}
}

func TestVerdictHelpers(t *testing.T) {
	cases := []struct{ a, b, max string }{
		{VerdictSafe, VerdictReview, VerdictReview},
		{VerdictRisky, VerdictReview, VerdictRisky},
		{VerdictSafe, VerdictSafe, VerdictSafe},
		{"", VerdictSafe, VerdictSafe},
		{"bogus", VerdictSafe, VerdictSafe},
	}
	for _, c := range cases {
		if got := MaxVerdict(c.a, c.b); got != c.max {
			t.Errorf("MaxVerdict(%q, %q) = %q, want %q", c.a, c.b, got, c.max)
		}
	}
	if VerdictRank("bogus") != -1 || VerdictRank(VerdictRisky) != 2 {
		t.Fatal("VerdictRank does not order safe < review < risky with unknown -1")
	}
	if got := VerdictOf([]Finding{{Severity: "bogus"}}, ParseAST); got != VerdictRisky {
		t.Fatalf("an unknown severity gives %s, want risky (fail closed)", got)
	}
	if got := VerdictOf(nil, "bogus"); got != VerdictReview {
		t.Fatalf("an unknown parse mode gives %s, want review", got)
	}
}
