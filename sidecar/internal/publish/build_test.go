package publish

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// pub-01: the outcome bundle's numbers, computed in code from ledger rows.
// No concurrency test: Build is a pure function of its input and shares
// no state. No state-transition test: a bundle is immutable once built.

var testNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func sampleBundle(t *testing.T) Bundle {
	t.Helper()
	rows := SampleRows()
	if len(rows) != 40 {
		t.Fatalf("the sample ledger has %d changes, want 40", len(rows))
	}
	return Build(Input{Rows: rows, Databases: 1, Since: 90 * 24 * time.Hour, Now: testNow})
}

func TestBuild_FortyChangesMatchTheHandCount(t *testing.T) {
	b := sampleBundle(t)
	want := Counts{Changes: 40, Improved: 21, NoChange: 12, Regressed: 3, Inconclusive: 3,
		Pending: 1, Reverted: 3, RollbackFailed: 1}
	if b.Totals == nil || *b.Totals != want {
		t.Fatalf("totals %+v, want %+v", b.Totals, want)
	}
	if b.Format != Format || b.KFloor != 5 || b.Since != "90d" || b.Databases != 1 {
		t.Fatalf("head %q k=%d since=%q databases=%d", b.Format, b.KFloor, b.Since,
			b.Databases)
	}
	if !b.From.Equal(testNow.Add(-90*24*time.Hour)) || !b.To.Equal(testNow) {
		t.Fatalf("window %s..%s, want 90 days ending %s", b.From, b.To, testNow)
	}
	if !strings.HasPrefix(b.Statement, "40 changes in the window") {
		t.Fatalf("statement %q, want it to lead with the 40 changes", b.Statement)
	}
	if got := b.RevertedByVerdict; len(got) != 2 || got[outcomes.VerdictImproved] != 1 ||
		got[outcomes.VerdictRegressed] != 2 {
		t.Fatalf("reverted by verdict %v, want improved 1 and regressed 2", got)
	}
}

func TestBuild_ClassesUnderTheFloorFoldIntoOther(t *testing.T) {
	b := sampleBundle(t)
	want := []Bucket{
		{Key: "create_index_concurrently", Counts: Counts{Changes: 12, Improved: 8,
			NoChange: 2, Regressed: 1, Inconclusive: 1, Reverted: 2}},
		{Key: "analyze_table", Counts: Counts{Changes: 10, Improved: 6, NoChange: 3,
			Pending: 1}},
		{Key: "vacuum_table", Counts: Counts{Changes: 7, Improved: 3, NoChange: 2,
			Inconclusive: 2}},
		{Key: "drop_unused_index", Counts: Counts{Changes: 5, NoChange: 4, Regressed: 1,
			Reverted: 1}},
		{Key: OtherKey, Counts: Counts{Changes: 6, Improved: 4, NoChange: 1, Regressed: 1,
			RollbackFailed: 1}},
	}
	assertBuckets(t, "by class", b.ByClass, want)
}

func TestBuild_OriginsUnderTheFloorFoldIntoOther(t *testing.T) {
	b := sampleBundle(t)
	if len(b.ByOrigin) != 3 || b.ByOrigin[0].Key != outcomes.OriginAutonomous ||
		b.ByOrigin[0].Changes != 24 || b.ByOrigin[1].Key != outcomes.OriginApproved ||
		b.ByOrigin[1].Changes != 11 || b.ByOrigin[2].Key != OtherKey ||
		b.ByOrigin[2].Changes != 5 {
		t.Fatalf("by origin %+v, want autonomous 24, approved 11, other 5 (plan 4 and "+
			"agent 1 under the floor)", b.ByOrigin)
	}
}

func assertBuckets(t *testing.T, what string, got, want []Bucket) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d buckets %+v, want %d", what, len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %+v, want %+v", what, i, got[i], want[i])
		}
	}
}

func rowsOf(actionType string, n int) []outcomes.Row {
	out := make([]outcomes.Row, n)
	for i := range out {
		out[i] = outcomes.Row{ActionType: actionType, Verdict: outcomes.VerdictImproved,
			Origin: outcomes.OriginAutonomous, RollbackState: outcomes.RollbackAvailable}
	}
	return out
}

func TestBuild_FloorBoundaryFourFoldsFiveStays(t *testing.T) {
	rows := append(rowsOf("analyze_table", 4), rowsOf("reindex_concurrently", 5)...)
	b := Build(Input{Rows: rows, Databases: 1, Since: 24 * time.Hour, Now: testNow})
	if len(b.ByClass) != 2 || b.ByClass[0].Key != "reindex_concurrently" ||
		b.ByClass[0].Changes != 5 || b.ByClass[1].Key != OtherKey ||
		b.ByClass[1].Changes != 4 {
		t.Fatalf("by class %+v, want reindex_concurrently 5 then other 4", b.ByClass)
	}
	if strings.Contains(mustJSON(t, b), "analyze_table") {
		t.Fatal("a class with 4 changes is named in the bundle")
	}
}

func TestBuild_UnknownActionTypesOriginsAndVerdictsBecomeFixedCodes(t *testing.T) {
	rows := rowsOf("drop_table_sk_live_51HxQ", 6)
	for i := range rows {
		rows[i].Origin = "origin_from_a_future_version"
		rows[i].Verdict = "verdict_from_a_future_version"
		rows[i].RollbackState = outcomes.RollbackRolledBack
	}
	b := Build(Input{Rows: rows, Databases: 1, Since: 24 * time.Hour, Now: testNow})
	text := mustJSON(t, b)
	for _, leak := range []string{"sk_live", "future_version"} {
		if strings.Contains(text, leak) {
			t.Fatalf("bundle carries %q: %s", leak, text)
		}
	}
	if len(b.ByClass) != 1 || b.ByClass[0].Key != OtherKey || len(b.ByOrigin) != 1 ||
		b.ByOrigin[0].Key != OtherKey {
		t.Fatalf("by class %+v by origin %+v, want one other bucket each", b.ByClass,
			b.ByOrigin)
	}
	if b.Totals.Inconclusive != 6 || b.RevertedByVerdict[UnknownVerdict] != 6 {
		t.Fatalf("totals %+v reverted %v, want an unknown verdict counted inconclusive",
			b.Totals, b.RevertedByVerdict)
	}
}

func TestBuild_LeavesOutDatabaseNamesFreeTextAndIDs(t *testing.T) {
	rows := rowsOf("create_index_concurrently", 5)
	for i := range rows {
		rows[i].Database = "acme_payroll_prod"
		rows[i].Why = "customer_email = 'ceo@acme.example' password=hunter2"
		rows[i].What = "CREATE INDEX ON acme_secret_table (ssn)"
		rows[i].EvidenceID = "ev-7f3a9c"
		rows[i].PackHash = "deadbeefcafe"
		rows[i].ActionID = 987654321
	}
	b := Build(Input{Rows: rows, Databases: 1, Since: 24 * time.Hour, Now: testNow})
	text := mustJSON(t, b) + string(RenderHTML(b, "0123456789abcdef"))
	for _, leak := range []string{"acme", "hunter2", "ceo@", "ssn", "ev-7f3a9c",
		"deadbeef", "987654321"} {
		if strings.Contains(text, leak) {
			t.Errorf("bundle carries %q", leak)
		}
	}
}

func TestBuild_EmptyLedgerSaysNoChangesNotZeros(t *testing.T) {
	for _, rows := range [][]outcomes.Row{nil, {}} {
		b := Build(Input{Rows: rows, Databases: 2, Since: 90 * 24 * time.Hour, Now: testNow})
		if b.Statement != NoChangesStatement || b.Totals != nil || b.ByClass != nil ||
			b.ByOrigin != nil || b.RevertedByVerdict != nil {
			t.Fatalf("empty bundle %+v, want the no-changes statement and no counts", b)
		}
		text := mustJSON(t, b)
		if !strings.Contains(text, `"totals": null`) || strings.Contains(text, `"changes"`) {
			t.Fatalf("empty bundle JSON presents counts: %s", text)
		}
		html := string(RenderHTML(b, ""))
		if !strings.Contains(html, "No changes in the window") ||
			strings.Contains(html, "<td>0</td>") {
			t.Fatalf("empty bundle page does not say there were no changes:\n%s", html)
		}
	}
}

func TestBuild_VerifierFalsePositiveRateAddsUpEveryBacktest(t *testing.T) {
	b := Build(Input{Rows: rowsOf("analyze_table", 5), Databases: 2, Since: time.Hour,
		Now: testNow, Backtests: []backtest.Counts{
			{Placebos: 300, Excluded: 20, FalsePositives: 7},
			{Placebos: 200, FalsePositives: 3}}})
	v := b.Verifier
	if v == nil || v.Backtests != 2 || v.UsablePlacebos != 480 || v.FalsePositives != 10 ||
		v.FPRate != 0.0208 {
		t.Fatalf("verifier %+v, want 2 back-tests, 10 of 480 usable = 0.0208", v)
	}
	if upper := backtest.WilsonUpper(10, 480); v.FPWilsonUpper < 0.0208 ||
		v.FPWilsonUpper != roundTo(upper, 4) {
		t.Fatalf("wilson upper %v, want %v rounded to 4 places", v.FPWilsonUpper, upper)
	}
	if b.VerifierStatement != "" {
		t.Fatalf("verifier statement %q beside a measured rate", b.VerifierStatement)
	}
}

func TestBuild_VerifierWithoutUsablePlacebosIsNotMeasured(t *testing.T) {
	for name, tests := range map[string][]backtest.Counts{
		"none":      nil,
		"no usable": {{Placebos: 10, Excluded: 10}},
	} {
		b := Build(Input{Since: time.Hour, Now: testNow, Backtests: tests})
		if b.Verifier != nil || b.VerifierStatement == "" {
			t.Errorf("%s: verifier %+v statement %q, want no rate and a reason", name,
				b.Verifier, b.VerifierStatement)
		}
	}
}

func TestParseSince(t *testing.T) {
	good := map[string]time.Duration{"90d": 90 * 24 * time.Hour, "1d": 24 * time.Hour,
		"400d": 400 * 24 * time.Hour, "36h": 36 * time.Hour}
	for raw, want := range good {
		if got, err := ParseSince(raw); err != nil || got != want {
			t.Errorf("ParseSince(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "0d", "-1d", "401d", "abc", "d", "1.5d", "0h", "90 d",
		"9999999999999999999d"} {
		if got, err := ParseSince(raw); err == nil {
			t.Errorf("ParseSince(%q) = %v, want an error", raw, got)
		}
	}
	if FormatSince(90*24*time.Hour) != "90d" || FormatSince(36*time.Hour) != "36h" {
		t.Fatalf("FormatSince: %q %q", FormatSince(90*24*time.Hour), FormatSince(36*time.Hour))
	}
}

func mustJSON(t *testing.T, b Bundle) string {
	t.Helper()
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
