package safetybench

import (
	"context"
	"strings"
	"testing"
	"time"
)

// These tests need a real PostgreSQL server (SAGE_TEST_DATABASE_URL).

// AQ-04 casts the masked people.ssn column. Under a READ ONLY transaction
// and under the read-only role the cast fails with a 22P02 whose message
// carries the value: the write checksums are intact, but the value reached
// the caller, so the cell is a leak (NOT HELD), not "not measured".
func TestAQ04LeaksUnderTxnAndRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	aq04 := corpusCase(t, "AQ-04")
	designs := []RODesign{readOnlyTxnDesign{}, privRoleDesign{roleName: readOnlyRole}}
	results, err := RunReadOnly(ctx, pool, []Case{aq04}, designs)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !results[0].Sentinel {
		t.Error("AQ-04's result must say it declares sentinels")
	}
	for _, a := range results[0].Attempts {
		if a.Observed != ClassLeakedInError || !a.ChecksumsIntact {
			t.Errorf("%s: observed %s, intact %v, detail %q; want leaked_in_error, intact",
				a.Design, a.Observed, a.ChecksumsIntact, a.Detail)
		}
		if a.Held() || a.NotMeasured() {
			t.Errorf("%s: held %v, n/m %v; want NOT HELD", a.Design, a.Held(),
				a.NotMeasured())
		}
		if strings.Contains(a.Detail, ssn) || !strings.Contains(a.Detail, "[sentinel]") {
			t.Errorf("%s: detail %q must carry the redaction, not the value", a.Design,
				a.Detail)
		}
	}
}

// With the leak scored, every design measures every corpus case: no cell is
// left "not measured" (benches-04 CHECK-02).
func TestEveryDesignMeasuresEveryCase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := newPool(ctx, t)
	cases, err := LoadCases(false)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	designs := Designs(readOnlyRole)
	defer closeDesigns(designs)
	results, err := RunReadOnly(ctx, pool, cases, designs)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	r := BuildReport(ReportMeta{Designs: designNames(designs)}, results, nil, nil)
	sum := r.ReadOnlySummary()
	for _, d := range designNames(designs) {
		s := sum[d]
		if s.NotMeasured != 0 || s.Total != len(cases) {
			t.Errorf("%s: %d not measured of %d (want 0 of %d)", d, s.NotMeasured, s.Total,
				len(cases))
		}
	}
	for _, c := range r.ReadOnly {
		for _, a := range c.Attempts {
			if a.NotMeasured() {
				t.Errorf("%s %s not measured: %q", a.Design, c.ID, a.Detail)
			}
		}
	}
}

func corpusCase(t *testing.T, id string) Case {
	t.Helper()
	cases, err := LoadCases(false)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	for _, c := range cases {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("corpus has no %s", id)
	return Case{}
}
