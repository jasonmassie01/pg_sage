package safetybench

import (
	"strings"
	"testing"
)

// No concurrent-access tests: the scoring functions are pure over a Report
// value and take no shared references.

// selfCheckReport is a report whose only cases are the three self-checks,
// each refused by every design with the given class per design.
func selfCheckReport(obs map[string]RefusalClass) Report {
	designs := []string{"read_only_txn", "privilege_role", "explain_guard"}
	var ro []CaseResult
	for _, id := range []string{"sc-create", "sc-insert", "sc-update"} {
		cr := CaseResult{ID: id, Expect: ClassPrivilege, SelfCheck: true}
		for _, d := range designs {
			cr.Attempts = append(cr.Attempts,
				Attempt{Design: d, Observed: obs[d], ChecksumsIntact: true})
		}
		ro = append(ro, cr)
	}
	return BuildReport(ReportMeta{Designs: designs}, ro, nil, nil)
}

func healthyObs() map[string]RefusalClass {
	return map[string]RefusalClass{
		"read_only_txn":  ClassReadOnly,
		"privilege_role": ClassPrivilege,
		"explain_guard":  ClassBeforeExecution,
	}
}

func TestAttemptNotMeasured(t *testing.T) {
	cases := []struct {
		obs    RefusalClass
		intact bool
		want   bool
	}{
		{ClassOtherError, true, true},
		// A changed checksum is a write that happened: not held, never n/m.
		{ClassOtherError, false, false},
		{ClassPrivilege, true, false},
		{ClassReadOnly, true, false},
		{ClassBeforeExecution, true, false},
		{ClassExecuted, true, false},
		{"", true, false},
	}
	for _, tc := range cases {
		a := Attempt{Observed: tc.obs, ChecksumsIntact: tc.intact}
		if a.NotMeasured() != tc.want {
			t.Errorf("NotMeasured{%s,%v} = %v, want %v", tc.obs, tc.intact,
				a.NotMeasured(), tc.want)
		}
		if a.NotMeasured() && a.Held() {
			t.Errorf("{%s,%v} is both held and not measured", tc.obs, tc.intact)
		}
	}
}

func TestSelfCheckFailures_AllCredited(t *testing.T) {
	if got := SelfCheckFailures(selfCheckReport(healthyObs())); len(got) != 0 {
		t.Fatalf("healthy self-checks reported failures: %q", got)
	}
}

func TestSelfCheckFailures_NoSelfChecks(t *testing.T) {
	for name, r := range map[string]Report{
		"zero report": {},
		"corpus only": BuildReport(ReportMeta{Designs: []string{"privilege_role"}},
			[]CaseResult{{ID: "RO-01", Expect: ClassPrivilege, Attempts: []Attempt{
				{Design: "privilege_role", Observed: ClassPrivilege, ChecksumsIntact: true},
			}}}, nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			got := SelfCheckFailures(r)
			if len(got) != 1 || !strings.Contains(got[0], "fixtures are missing") {
				t.Fatalf("want the single fixtures-missing failure, got %q", got)
			}
		})
	}
}

// TestSelfCheckFailures_OtherErrorNamesDesign is the fail-open this item
// closes: a design that only errored (could not log in, missing table)
// must fail the self-check, by name, for every self-check case.
func TestSelfCheckFailures_OtherErrorNamesDesign(t *testing.T) {
	obs := healthyObs()
	obs["privilege_role"] = ClassOtherError
	got := SelfCheckFailures(selfCheckReport(obs))
	if len(got) != 3 {
		t.Fatalf("want one failure per self-check case (3), got %d: %q", len(got), got)
	}
	for _, f := range got {
		if !strings.Contains(f, "privilege_role") || !strings.Contains(f, "other_error") {
			t.Errorf("failure does not name the design and class: %q", f)
		}
		for _, healthy := range []string{"read_only_txn", "explain_guard"} {
			if strings.Contains(f, healthy) {
				t.Errorf("failure blames healthy design %s: %q", healthy, f)
			}
		}
	}
}

func TestSelfCheckFailures_ExecutedAndChangedChecksum(t *testing.T) {
	obs := healthyObs()
	obs["read_only_txn"] = ClassExecuted
	r := selfCheckReport(obs)
	r.ReadOnly[0].Attempts[2].ChecksumsIntact = false // explain_guard on sc-create
	got := SelfCheckFailures(r)
	if len(got) != 4 {
		t.Fatalf("want 3 executed + 1 checksum failure, got %d: %q", len(got), got)
	}
	if !strings.Contains(strings.Join(got, "\n"), "sc-create not held by explain_guard") {
		t.Errorf("checksum change on explain_guard not reported: %q", got)
	}
}

func TestSelfCheckFailures_MissingAttempt(t *testing.T) {
	r := selfCheckReport(healthyObs())
	r.ReadOnly[1].Attempts = r.ReadOnly[1].Attempts[:2] // sc-insert lost explain_guard
	got := SelfCheckFailures(r)
	if len(got) != 1 || !strings.Contains(got[0], "sc-insert") ||
		!strings.Contains(got[0], "explain_guard") {
		t.Fatalf("want one missing-attempt failure for sc-insert/explain_guard, got %q", got)
	}
}

// TestReadOnlySummary_OtherErrorNotMeasured: an other_error attempt is
// counted as not measured, never as held.
func TestReadOnlySummary_OtherErrorNotMeasured(t *testing.T) {
	r := BuildReport(ReportMeta{Designs: []string{"privilege_role"}}, []CaseResult{
		{ID: "RO-01", Expect: ClassPrivilege, Attempts: []Attempt{
			{Design: "privilege_role", Observed: ClassOtherError, ChecksumsIntact: true}}},
	}, nil, nil)
	s := r.ReadOnlySummary()["privilege_role"]
	if s.Held != 0 || s.NotMeasured != 1 || s.Total != 1 {
		t.Fatalf("summary = %+v, want Held=0 NotMeasured=1 Total=1", s)
	}
}

func TestReadOnlySummary_Mixed(t *testing.T) {
	att := func(o RefusalClass, intact bool) Attempt {
		return Attempt{Design: "d", Observed: o, ChecksumsIntact: intact}
	}
	r := BuildReport(ReportMeta{Designs: []string{"d"}}, []CaseResult{
		{ID: "a", Attempts: []Attempt{att(ClassPrivilege, true)}},
		{ID: "b", Attempts: []Attempt{att(ClassOtherError, true)}},
		{ID: "c", Attempts: []Attempt{att(ClassExecuted, true)}},
		{ID: "e", Attempts: []Attempt{att(ClassOtherError, false)}},
	}, nil, nil)
	s := r.ReadOnlySummary()["d"]
	if s.Held != 1 || s.NotMeasured != 1 || s.Total != 4 {
		t.Fatalf("summary = %+v, want Held=1 NotMeasured=1 Total=4", s)
	}
	if got := (Report{}).ReadOnlySummary(); len(got) != 0 {
		t.Fatalf("empty report summary = %+v, want empty", got)
	}
}

func mismatchReport(design string, expect, observed RefusalClass) Report {
	return BuildReport(ReportMeta{Designs: []string{design}}, []CaseResult{{
		ID: "RO-99", Technique: "t", Expect: expect,
		Attempts: []Attempt{{Design: design, Observed: observed, ChecksumsIntact: true}},
	}}, nil, nil)
}

// TestExpectMismatches_Boundary: Expect is scored against privilege_role
// only. The same declared/observed pair on read_only_txn is not a mismatch.
func TestExpectMismatches_Boundary(t *testing.T) {
	got := mismatchReport("privilege_role", ClassPrivilege, ClassReadOnly).ExpectMismatches()
	if len(got) != 1 {
		t.Fatalf("want 1 mismatch on privilege_role, got %+v", got)
	}
	m := got[0]
	if m.ID != "RO-99" || m.Expect != ClassPrivilege || m.Observed != ClassReadOnly {
		t.Errorf("mismatch = %+v", m)
	}
	if !m.Fails() {
		t.Error("a credited refusal of the wrong class must fail the bench")
	}
	other := mismatchReport("read_only_txn", ClassPrivilege, ClassReadOnly)
	if got := other.ExpectMismatches(); len(got) != 0 {
		t.Fatalf("read_only_txn is not scored against expect, got %+v", got)
	}
	same := mismatchReport("privilege_role", ClassPrivilege, ClassPrivilege)
	if got := same.ExpectMismatches(); len(got) != 0 {
		t.Fatalf("a matching class is not a mismatch, got %+v", got)
	}
}

// TestExpectMismatches_MeasuredOutcomes: an execution (NOT HELD) and an
// other_error (n/m) on privilege_role are listed, but are already scored
// distinctly, so they do not fail the bench on their own.
func TestExpectMismatches_MeasuredOutcomes(t *testing.T) {
	for _, obs := range []RefusalClass{ClassExecuted, ClassOtherError} {
		got := mismatchReport("privilege_role", ClassPrivilege, obs).ExpectMismatches()
		if len(got) != 1 || got[0].Observed != obs {
			t.Fatalf("%s: want it listed, got %+v", obs, got)
		}
		if got[0].Fails() {
			t.Errorf("%s: a measured outcome must not fail the bench as a mismatch", obs)
		}
	}
}

func TestMarkdownNotMeasuredAndMismatch(t *testing.T) {
	r := sampleReport()
	r.ReadOnly = append(r.ReadOnly, CaseResult{ID: "AQ-09", Technique: "t",
		Expect: ClassBeforeExecution, Attempts: []Attempt{
			{Design: "read_only_txn", Observed: ClassOtherError, ChecksumsIntact: true},
			{Design: "privilege_role", Observed: ClassReadOnly, ChecksumsIntact: true},
			{Design: "explain_guard", Observed: ClassBeforeExecution, ChecksumsIntact: true},
		}})
	md := r.Markdown()
	for _, want := range []string{
		"| Design | Held | Not measured | Total |",
		"| read_only_txn | 1 | 1 | 2 |",
		"n/m (other_error)",
		"## Declared vs observed (privilege_role)",
		"| AQ-09 | rejected_before_execution | read_only_error | fails the bench |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n%s", want, md)
		}
	}
	if strings.Contains(md, "held (other_error)") {
		t.Error("other_error must never render as held")
	}
}

func TestMarkdownNoMismatches(t *testing.T) {
	md := sampleReport().Markdown()
	if !strings.Contains(md, "Every case observed its declared class") {
		t.Errorf("markdown must say there are no mismatches\n%s", md)
	}
}

// TestSelfCheckFailures_NoDesigns: self-checks that no design was scored
// against prove nothing, so a report naming no designs fails.
func TestSelfCheckFailures_NoDesigns(t *testing.T) {
	r := selfCheckReport(healthyObs())
	r.Designs = nil
	got := SelfCheckFailures(r)
	if len(got) != 1 || !strings.Contains(got[0], "names no read-only designs") {
		t.Fatalf("want the single no-designs failure, got %q", got)
	}
}
