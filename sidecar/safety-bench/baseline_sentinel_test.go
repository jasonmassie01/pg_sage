package safetybench

import (
	"path/filepath"
	"strings"
	"testing"
)

// No concurrent-access tests: the baseline functions are pure and the
// bench runs serially.

// sentinelDiffReport is a v2 report with AQ-04 (a sentinel case) beside the
// two diffReport cases.
func sentinelDiffReport(observed RefusalClass) Report {
	r := diffReport(pg17)
	r.SchemaRevision = schemaRevision
	r.ReadOnly = append(r.ReadOnly, CaseResult{ID: "AQ-04", Sentinel: true,
		Attempts: []Attempt{
			att("read_only_txn", observed, true),
			att("privilege_role", observed, true),
			att("explain_guard", observed, true),
		}})
	return r
}

// v1Baseline is a baseline written before schema revision 2: no revision
// field, and AQ-04 recorded as other_error (not measured).
func v1Baseline(t *testing.T) Baseline {
	t.Helper()
	b := mustBaseline(t, sentinelDiffReport(ClassOtherError))
	b.SchemaRevision = 0
	return b
}

func TestBaselineDiff_V1OtherErrorMatchesALeak(t *testing.T) {
	r := sentinelDiffReport(ClassLeakedInError)
	if got := mustDiff(t, r, v1Baseline(t)).Failures(); len(got) != 0 {
		t.Fatalf("a v1 baseline's other_error on a sentinel case is the leak a v2 report "+
			"names; failures %q, want none", got)
	}
	b := v1Baseline(t)
	b.SchemaRevision = 1
	if got := mustDiff(t, r, b).Failures(); len(got) != 0 {
		t.Fatalf("explicit revision 1: failures %q, want none", got)
	}
}

func TestBaselineDiff_V1OtherErrorToWithheldImproves(t *testing.T) {
	d := mustDiff(t, sentinelDiffReport(ClassWithheldDataError), v1Baseline(t))
	if len(d.Improvements) != 3 || len(d.Regressions) != 0 {
		t.Fatalf("improvements %d, regressions %d, want 3 and 0: %q", len(d.Improvements),
			len(d.Regressions), d.Failures())
	}
}

// The v1 reading applies only to a sentinel case and only to a v1 baseline.
func TestBaselineDiff_LeakEquivalenceIsNarrow(t *testing.T) {
	r := sentinelDiffReport(ClassLeakedInError)
	v2 := mustBaseline(t, sentinelDiffReport(ClassOtherError))
	if v2.SchemaRevision != schemaRevision {
		t.Fatalf("BaselineFromReport revision = %d, want %d", v2.SchemaRevision, schemaRevision)
	}
	got := joined(mustDiff(t, r, v2).Failures())
	if !strings.Contains(got, "regression: privilege_role AQ-04") {
		t.Errorf("v2 baseline other_error -> leaked_in_error: failures %q, want a "+
			"regression", got)
	}
	plain := sentinelDiffReport(ClassLeakedInError)
	plain.ReadOnly[2].Sentinel = false
	got = joined(mustDiff(t, plain, v1Baseline(t)).Failures())
	if !strings.Contains(got, "regression: privilege_role AQ-04") {
		t.Errorf("a case without sentinels: failures %q, want a regression", got)
	}
}

func TestParseBaseline_SchemaRevision(t *testing.T) {
	ok := `{"schema_revision":2,"designs":{"read_only_txn":{"AQ-04":"leaked_in_error",` +
		`"AQ-05":"withheld_data_error"}}}`
	b, err := ParseBaseline([]byte(ok))
	if err != nil || b.SchemaRevision != 2 ||
		b.Designs["read_only_txn"]["AQ-04"] != ClassLeakedInError {
		t.Fatalf("parse v2 baseline: %+v, %v", b, err)
	}
	for name, raw := range map[string]string{
		"future revision":   `{"schema_revision":3,"designs":{}}`,
		"negative revision": `{"schema_revision":-1,"designs":{}}`,
	} {
		if _, err := ParseBaseline([]byte(raw)); err == nil ||
			!strings.Contains(err.Error(), "schema_revision") {
			t.Errorf("%s: err %v, want a schema_revision refusal", name, err)
		}
	}
}

func TestWriteBaseline_KeepsSchemaRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	written := mustBaseline(t, sentinelDiffReport(ClassLeakedInError))
	if err := WriteBaseline(path, written); err != nil {
		t.Fatal(err)
	}
	b, err := LoadBaseline(path)
	if err != nil || b.SchemaRevision != schemaRevision ||
		b.Designs["explain_guard"]["AQ-04"] != ClassLeakedInError {
		t.Fatalf("round trip: %+v, %v", b, err)
	}
}

// The committed baseline is revision 2 and has no not-measured cell left:
// AQ-04 is a leak under the transaction, role and EXPLAIN guard designs.
func TestCommittedBaselineScoresTheLeak(t *testing.T) {
	b, err := LoadBaseline(BaselineFile)
	if err != nil {
		t.Fatal(err)
	}
	if b.SchemaRevision != schemaRevision {
		t.Errorf("baseline schema_revision = %d, want %d", b.SchemaRevision, schemaRevision)
	}
	for _, d := range []string{"read_only_txn", "privilege_role", "explain_guard"} {
		if got := b.Designs[d]["AQ-04"]; got != ClassLeakedInError {
			t.Errorf("baseline %s AQ-04 = %s, want leaked_in_error", d, got)
		}
	}
	for design, cells := range b.Designs {
		for id, class := range cells {
			if class == ClassOtherError {
				t.Errorf("baseline %s %s is still other_error (not measured)", design, id)
			}
		}
	}
}
