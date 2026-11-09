package agentposture

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// No concurrent access tests here: one monitor runs posture serially per
// database, and Input, rowCap and the grouping helpers are per-run values
// that no two goroutines share.

// truncDetector reports each of truncs through its input, then returns
// findings or err.
type truncDetector struct {
	spec     Spec
	truncs   []Truncation
	findings []Finding
	err      error
}

func (d *truncDetector) Spec() Spec { return d.spec }

func (d *truncDetector) Detect(_ context.Context, in Input) ([]Finding, error) {
	for _, tr := range d.truncs {
		in.Truncated(tr.Arm, tr.Limit)
	}
	return d.findings, d.err
}

func newTruncDetector(id string, truncs ...Truncation) *truncDetector {
	return &truncDetector{spec: Spec{ID: id, Title: "trunc " + id, Severity: Critical},
		truncs: truncs, findings: []Finding{okFinding(id, Critical)}}
}

func TestRunDetector_RecordsTruncationAndKeepsFindings(t *testing.T) {
	d := newTruncDetector("AP-03", Truncation{Arm: "relations", Limit: 50000})
	out, err := RunDetector(context.Background(), d, nil, Env{VersionNum: 170000})
	if err != nil {
		t.Fatalf("RunDetector: %v", err)
	}
	if !slices.Equal(out.Truncated, []Truncation{{Arm: "relations", Limit: 50000}}) {
		t.Fatalf("truncated = %+v, want relations at 50000", out.Truncated)
	}
	if len(out.Findings) != 1 || out.Findings[0].Object != "public.t" {
		t.Fatalf("findings = %+v, want the rows that were read", out.Findings)
	}
	want := "relations: first 50,000 rows read, later rows not checked"
	if got := out.Note(); got != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
}

func TestRunDetector_TruncationThenErrorReturnsNothing(t *testing.T) {
	d := newTruncDetector("AP-03", Truncation{Arm: "relations", Limit: 3})
	d.err = errors.New("connection lost")
	out, err := RunDetector(context.Background(), d, nil, Env{})
	if err == nil || !strings.Contains(err.Error(), "AP-03") {
		t.Fatalf("err = %v, want the detector's error naming AP-03", err)
	}
	if len(out.Truncated) != 0 || len(out.Findings) != 0 {
		t.Fatalf("outcome = %+v, want nothing from a failed run", out)
	}
}

func TestRunDetector_NoTruncationLeavesNoteEmpty(t *testing.T) {
	out, err := RunDetector(context.Background(), newTruncDetector("AP-05"), nil, Env{})
	if err != nil || out.Truncated != nil || out.Note() != "" {
		t.Fatalf("outcome = %+v, %v; note %q, want no truncation", out, err, out.Note())
	}
}

func TestRunDetector_TwoTruncatedArmsAreBothKept(t *testing.T) {
	d := newTruncDetector("AP-15", Truncation{Arm: "PUBLIC grants", Limit: 5000},
		Truncation{Arm: "default privileges", Limit: 5000})
	out, err := RunDetector(context.Background(), d, nil, Env{})
	if err != nil || len(out.Truncated) != 2 || out.Truncated[1].Arm != "default privileges" {
		t.Fatalf("outcome = %+v, %v, want both arms in order", out, err)
	}
}

func TestOutcomeNote_SkipsThenTruncations(t *testing.T) {
	o := Outcome{Detector: "AP-06", Skipped: []ArmSkip{{Arm: "security_invoker",
		Reason: "PostgreSQL 15+"}}, Truncated: []Truncation{{Arm: "views", Limit: 3}}}
	want := "arm security_invoker skipped: PostgreSQL 15+; " +
		"views: first 3 rows read, later rows not checked"
	if got := o.Note(); got != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
}

func TestResult_TruncatedDetectorsAreNotEvaluated(t *testing.T) {
	trunc := []Truncation{{Arm: "relations", Limit: 3}}
	r := Result{Outcomes: []Outcome{
		{Detector: "AP-01", Findings: []Finding{okFinding("AP-01", Warning)}},
		{Detector: "AP-03", Findings: []Finding{okFinding("AP-03", Critical)},
			Truncated: trunc},
		{Detector: "AP-05"},
	}}
	if got := r.EvaluatedCategories(); !slices.Equal(got,
		[]string{"agent_posture:AP-01", "agent_posture:AP-05"}) {
		t.Fatalf("evaluated = %v, want AP-01 and AP-05 only", got)
	}
	if got := r.TruncatedCategories(); !slices.Equal(got, []string{"agent_posture:AP-03"}) {
		t.Fatalf("truncated = %v, want AP-03", got)
	}
	if got := r.Findings(); len(got) != 2 || got[1].Detector != "AP-03" {
		t.Fatalf("findings = %+v, want the truncated detector's findings kept", got)
	}
}

func TestResult_EmptyHasNoTruncatedCategories(t *testing.T) {
	if got := (Result{}).TruncatedCategories(); len(got) != 0 {
		t.Fatalf("truncated = %v, want none", got)
	}
}

func TestInput_TruncatedOutsideARunIsIgnored(t *testing.T) {
	Input{}.Truncated("relations", 3) // must not panic
	var in Input
	c := in.capRows("relations", 1)
	first, second := c.past(), c.past()
	if first || !second {
		t.Fatalf("past = %v, %v; want the cap to count rows without a run to report to",
			first, second)
	}
}

func TestRowCap_ReadsOneRowPastTheLimit(t *testing.T) {
	var got []Truncation
	in := Input{truncated: &got}
	c := in.capRows("relations", 3)
	if c.fetch() != 4 {
		t.Fatalf("fetch = %d, want limit+1 = 4", c.fetch())
	}
	for i := 1; i <= 3; i++ {
		if c.past() {
			t.Fatalf("row %d is within the limit of 3", i)
		}
	}
	if len(got) != 0 {
		t.Fatalf("truncated %v before a row past the limit", got)
	}
	if !c.past() || !c.past() {
		t.Fatal("rows past the limit were not reported")
	}
	if !slices.Equal(got, []Truncation{{Arm: "relations", Limit: 3}}) {
		t.Fatalf("truncated = %v, want relations at 3 recorded once", got)
	}
}

func TestRowCap_ZeroAndNegativeLimits(t *testing.T) {
	for _, limit := range []int{0, -5} {
		var got []Truncation
		c := Input{truncated: &got}.capRows("roles", limit)
		if c.fetch() != 1 {
			t.Fatalf("limit %d: fetch = %d, want 1", limit, c.fetch())
		}
		if !c.past() || len(got) != 1 || got[0].Limit != 0 {
			t.Fatalf("limit %d: truncated = %v, want the first row past a 0 limit", limit,
				got)
		}
	}
}

type groupItem struct{ schema, name string }

func groupFindings(items []groupItem) ([]Finding, map[string][]string) {
	calls := map[string][]string{}
	out := groupBySchema(items, func(i groupItem) string { return i.schema },
		func(i groupItem) Finding {
			return Finding{ObjectType: "table", Object: i.schema + "." + i.name}
		},
		func(schema string, is []groupItem) Finding {
			for _, i := range is {
				calls[schema] = append(calls[schema], i.name)
			}
			return Finding{ObjectType: "schema", Object: schema}
		})
	return out, calls
}

func items(schema string, n int) []groupItem {
	out := make([]groupItem, n)
	for i := range out {
		out[i] = groupItem{schema: schema, name: fmt.Sprintf("t%02d", i+1)}
	}
	return out
}

func TestGroupBySchema_Threshold(t *testing.T) {
	out, calls := groupFindings(items("a", groupThreshold))
	if len(out) != 20 || out[19].Object != "a.t20" || len(calls) != 0 {
		t.Fatalf("20 items: %d findings, grouped %v; want 20 per-object", len(out), calls)
	}
	out, calls = groupFindings(items("a", groupThreshold+1))
	if len(out) != 1 || out[0].Object != "a" || out[0].ObjectType != "schema" {
		t.Fatalf("21 items: %+v, want one schema finding", out)
	}
	if len(calls["a"]) != 21 || calls["a"][0] != "t01" || calls["a"][20] != "t21" {
		t.Fatalf("grouped items = %v, want all 21 in order", calls["a"])
	}
}

func TestGroupBySchema_MixedSchemasKeepOrder(t *testing.T) {
	in := append(items("b", 2), items("a", 25)...)
	in = append(in, groupItem{schema: "b", name: "late"})
	out, calls := groupFindings(in)
	var objs []string
	for _, f := range out {
		objs = append(objs, f.Object)
	}
	if !slices.Equal(objs, []string{"b.t01", "b.t02", "a", "b.late"}) {
		t.Fatalf("objects = %v, want b's per-object findings around a's group", objs)
	}
	if len(calls["a"]) != 25 || calls["b"] != nil {
		t.Fatalf("grouped = %v, want only a", calls)
	}
}

func TestGroupBySchema_Empty(t *testing.T) {
	if out, calls := groupFindings(nil); len(out) != 0 || len(calls) != 0 {
		t.Fatalf("no items: %v, %v", out, calls)
	}
}

func TestGroupEvidence_CountAndFirstFifty(t *testing.T) {
	objs := make([]Evidence, 60)
	for i := range objs {
		objs[i] = Evidence{Source: "pg_proc", Ref: fmt.Sprintf("s.f%02d()", i+1)}
	}
	ev := groupEvidence("pg_proc", "s", "definer functions", objs)
	if len(ev) != 1+groupShown {
		t.Fatalf("evidence = %d entries, want the count and %d objects", len(ev), groupShown)
	}
	if ev[0].Ref != "s" || ev[0].Detail != "60 definer functions; the first 50 follow" {
		t.Fatalf("count entry = %+v", ev[0])
	}
	if ev[1].Ref != "s.f01()" || ev[50].Ref != "s.f50()" {
		t.Fatalf("listed %s .. %s, want f01 .. f50", ev[1].Ref, ev[50].Ref)
	}
	ev = groupEvidence("pg_proc", "s", "definer functions", objs[:25])
	if len(ev) != 26 || ev[0].Detail != "25 definer functions" {
		t.Fatalf("25 objects: %d entries, count %q", len(ev), ev[0].Detail)
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 3: "3", 999: "999", 1000: "1,000",
		50000: "50,000", 1234567: "1,234,567", -1200: "-1,200"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDollarQuoted_AvoidsTagsInTheBody(t *testing.T) {
	if got := dollarQuoted("SELECT 1;"); got != "$fix$\nSELECT 1;\n$fix$" {
		t.Fatalf("plain body = %q", got)
	}
	got := dollarQuoted("SELECT '$fix$';")
	if got != "$fix1$\nSELECT '$fix$';\n$fix1$" {
		t.Fatalf("body with the tag = %q, want $fix1$", got)
	}
}

func TestSQLLiteral(t *testing.T) {
	if got := sqlLiteral(`it's "x"`); got != `'it''s "x"'` {
		t.Fatalf("literal = %s", got)
	}
}
