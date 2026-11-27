package outcomes

import (
	"encoding/json"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ux-02a: argument parsing, the verdict and rollback vocabularies, the
// effect arithmetic and the summary. outcomes_db_test.go reads real rows.
// No concurrent access tests here: these are pure functions over values
// (the recorder's concurrency is covered in metrics_test.go).

func TestParseSince_AcceptsTheRetentionRangeAndRefusesTheRest(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 24 * time.Hour,
		"24h": 24 * time.Hour, "1s": time.Second, "90m": 90 * time.Minute,
		"9600h": 9600 * time.Hour} {
		got, err := ParseSince(raw)
		if err != nil || got != want {
			t.Errorf("ParseSince(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"abc", "-1h", "0h", "0", "9601h", "9600h1s", "24", "1d"} {
		_, err := ParseSince(raw)
		if err == nil {
			t.Errorf("ParseSince(%q) accepted", raw)
			continue
		}
		if !strings.Contains(err.Error(), "1s") || !strings.Contains(err.Error(), "9600h") {
			t.Errorf("ParseSince(%q) error %q does not give the allowed range", raw, err)
		}
	}
}

func TestParseVerdictAndLimit(t *testing.T) {
	for _, v := range []string{"", "pending", "improved", "no_change", "regressed",
		"inconclusive", "unverifiable", "expired"} {
		if got, err := ParseVerdict(v); err != nil || got != v {
			t.Errorf("ParseVerdict(%q) = %q, %v", v, got, err)
		}
	}
	for _, v := range []string{"bogus", "neutral", "insufficient_evidence", "IMPROVED"} {
		if _, err := ParseVerdict(v); err == nil || !strings.Contains(err.Error(), "no_change") {
			t.Errorf("ParseVerdict(%q) = %v, want an error listing the verdicts", v, err)
		}
	}
	if n, err := ParseLimit(""); err != nil || n != DefaultLimit {
		t.Fatalf("ParseLimit(\"\") = %d, %v", n, err)
	}
	if n, err := ParseLimit("10000"); err != nil || n != 10000 {
		t.Fatalf("ParseLimit(10000) = %d, %v", n, err)
	}
	for _, raw := range []string{"0", "-1", "10001", "ten"} {
		if _, err := ParseLimit(raw); err == nil || !strings.Contains(err.Error(), "1 to 10000") {
			t.Errorf("ParseLimit(%q) = %v", raw, err)
		}
	}
}

func TestLabel_MatchesTheDashboardsActionLabels(t *testing.T) {
	if got := Label("create_index_concurrently"); got != "Add an index" {
		t.Fatalf("Label(create_index_concurrently) = %q", got)
	}
	if got := Label("set_fillfactor"); got != "Set fillfactor" {
		t.Fatalf("fallback label = %q, want the type in words", got)
	}
	if got := Label(""); got != "Change details unavailable" {
		t.Fatalf("empty label = %q", got)
	}
	src, err := os.ReadFile("../../web/src/pages/actions/workflow.js")
	if err != nil {
		t.Fatalf("read the dashboard's labels: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "export function actionLabel")
	end := strings.Index(body[start:], "if (labels[value])")
	if start < 0 || end < 0 {
		t.Fatal("actionLabel not found in workflow.js")
	}
	pairs := regexp.MustCompile(`(\w+): '([^']+)'`).FindAllStringSubmatch(
		body[start:start+end], -1)
	if len(pairs) < 10 {
		t.Fatalf("found %d labels in workflow.js, want at least 10", len(pairs))
	}
	for _, p := range pairs {
		if got := Label(p[1]); got != p[2] {
			t.Errorf("Label(%s) = %q, the dashboard says %q", p[1], got, p[2])
		}
	}
}

func TestVerdictOf_MapsTheVerifierIntoTheLedgerVocabulary(t *testing.T) {
	for _, c := range []struct{ outcome, step, action, want string }{
		{"", "", "success", VerdictPending},
		{"pending", "", "success", VerdictPending},
		{"improved", "", "success", VerdictImproved},
		{"neutral", "", "success", VerdictNoChange},
		{"regressed", "", "rolled_back", VerdictRegressed},
		{"insufficient_evidence", "", "success", VerdictInconclusive},
		{"unverifiable", "", "success", VerdictUnverifiable},
		{"", "", "verification_expired", VerdictExpired},
		{"pending", "expired", "success", VerdictExpired},
		{"", "no_change", "success", VerdictNoChange},
		{"improved", "regressed", "success", VerdictImproved}, // the verifier's record wins
	} {
		if got := verdictOf(c.outcome, c.step, c.action); got != c.want {
			t.Errorf("verdictOf(%q, %q, %q) = %q, want %q", c.outcome, c.step, c.action,
				got, c.want)
		}
	}
}

func TestRollbackStateOf(t *testing.T) {
	for _, c := range []struct{ outcome, rollback, want string }{
		{"success", "DROP INDEX CONCURRENTLY i", RollbackAvailable},
		{"monitoring", "DROP INDEX i", RollbackAvailable},
		{"success", "", RollbackNotReversible},
		{"success", "   ", RollbackNotReversible},
		{"rolled_back", "DROP INDEX i", RollbackRolledBack},
		{"reverted", "", RollbackRolledBack},
		{"rollback_failed", "DROP INDEX i", RollbackFailed},
		{"rolling_back", "DROP INDEX i", RollbackInProgress},
	} {
		if got := rollbackStateOf(c.outcome, c.rollback); got != c.want {
			t.Errorf("rollbackStateOf(%q, %q) = %q, want %q", c.outcome, c.rollback, got, c.want)
		}
	}
}

func TestMeasuredOf_ComputesTheEffectOrSaysWhyNot(t *testing.T) {
	pct := func(e *Effect) float64 {
		if e == nil {
			t.Fatal("no measured effect")
		}
		return e.ChangePct
	}
	got, why := measuredOf(VerdictImproved, []byte(`{"metric":"shared_blks_per_call",`+
		`"before":100,"after":58,"change_pct":-42}`), "")
	if pct(got) != -42 || got.Metric != "shared_blks_per_call" || why != "" {
		t.Fatalf("stored change: %+v %q", got, why)
	}
	got, _ = measuredOf(VerdictRegressed, []byte(`{"metric":"mean_exec_time",`+
		`"before":3,"after":4}`), "")
	if math.Abs(pct(got)-33.3) > 1e-9 {
		t.Fatalf("computed change = %v, want 33.3 (one decimal)", got.ChangePct)
	}
	for _, c := range []struct {
		verdict, observed, reason, want string
	}{
		{VerdictImproved, `{"metric":"m","before":0,"after":5}`, "", "no baseline"},
		{VerdictImproved, `{}`, "", "no measurement"},
		{VerdictImproved, ``, "", "no measurement"},
		{VerdictImproved, `not json`, "", "no measurement"},
		{VerdictUnverifiable, `{"before":1,"after":2}`, "suspend epoch break",
			"suspend epoch break"},
		{VerdictUnverifiable, `{}`, "", "cannot be measured"},
		{VerdictInconclusive, `{}`, "too few calls", "too few calls"},
		{VerdictPending, `{}`, "", "not judged yet"},
		{VerdictExpired, `{}`, "", "deadline"},
	} {
		got, why := measuredOf(c.verdict, []byte(c.observed), c.reason)
		if got != nil || !strings.Contains(why, c.want) {
			t.Errorf("measuredOf(%s, %s) = %+v, %q; want none and %q", c.verdict,
				c.observed, got, why, c.want)
		}
	}
}

func TestExpectedOf_PrefersThePredictionThenThePlanStep(t *testing.T) {
	got := expectedOf([]byte(`{"class":"index_create","method":"hypopg",`+
		`"metric":"mean_exec_time","expected_change_pct":-35}`), stepExpect{})
	if got == nil || got.ChangePct != -35 || got.Metric != "mean_exec_time" ||
		got.Source != "hypopg" {
		t.Fatalf("prediction: %+v", got)
	}
	down, up := 20.0, 15.0
	got = expectedOf([]byte(`{"class":"guc","method":"none"}`),
		stepExpect{pct: &down, direction: "down", metric: "temp_bytes", source: "clone"})
	if got == nil || got.ChangePct != -20 || got.Source != "clone" || got.Metric != "temp_bytes" {
		t.Fatalf("plan step down: %+v", got)
	}
	got = expectedOf(nil, stepExpect{pct: &up, direction: "up", metric: "hot", source: "none"})
	if got == nil || got.ChangePct != 15 {
		t.Fatalf("plan step up: %+v", got)
	}
	if got := expectedOf(nil, stepExpect{pct: &up, direction: "none"}); got != nil {
		t.Fatalf("direction none: %+v, want no expectation", got)
	}
	if got := expectedOf([]byte(`{"method":"model"}`), stepExpect{}); got != nil {
		t.Fatalf("a prediction without a change: %+v", got)
	}
}

func TestSummarize_CountsAndTheMeasuredShare(t *testing.T) {
	m := &Effect{Metric: "m", ChangePct: -10}
	rows := []Row{
		{Verdict: VerdictImproved, Measured: m, RollbackState: RollbackAvailable},
		{Verdict: VerdictUnverifiable, RollbackState: RollbackNotReversible},
		{Verdict: VerdictPending, RollbackState: RollbackRolledBack},
	}
	s := Summarize(rows)
	if s.Changes != 3 || s.Verified != 1 || s.Inconclusive != 1 || s.Pending != 1 ||
		s.Reverted != 1 || s.Measured != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if s.MeasuredShare == nil || *s.MeasuredShare != 0.333 {
		t.Fatalf("measured share = %v, want 0.333", s.MeasuredShare)
	}
	empty := Summarize(nil)
	if empty.Changes != 0 || empty.MeasuredShare != nil {
		t.Fatalf("empty summary = %+v, want no share (null, not 0)", empty)
	}
	raw, _ := json.Marshal(empty)
	if !strings.Contains(string(raw), `"measured_share":null`) {
		t.Fatalf("empty summary JSON %s, want measured_share null", raw)
	}
	all := Summarize([]Row{{Verdict: VerdictRegressed, Measured: m},
		{Verdict: VerdictNoChange, Measured: m}, {Verdict: VerdictExpired}})
	if all.Verified != 2 || all.Inconclusive != 1 || *all.MeasuredShare != 0.667 {
		t.Fatalf("summary = %+v share %v", all, *all.MeasuredShare)
	}
}

func TestMerge_CombinesDatabasesNewestFirstAndKeepsErrors(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	m := &Effect{ChangePct: -5}
	aRows := []Row{{ActionID: 1, Database: "a", ExecutedAt: t0, Measured: m,
		Verdict: VerdictImproved}}
	a := Result{Rows: aRows, Summary: Summarize(aRows)}
	bRows := []Row{{ActionID: 7, Database: "b", ExecutedAt: t0.Add(time.Hour),
		Verdict: VerdictPending}, {ActionID: 2, Database: "b", ExecutedAt: t0,
		Verdict: VerdictPending}}
	b := Result{Rows: bRows, Summary: Summarize(bRows), Truncated: true}
	bad := Result{Errors: []ReadError{{Database: "c", Error: "read outcomes: c: refused"}}}
	got := Merge(2, a, b, bad)
	if len(got.Rows) != 2 || got.Rows[0].ActionID != 7 || got.Rows[1].Database != "a" {
		t.Fatalf("rows = %+v, want b/7 then a/1 (same time: database order)", got.Rows)
	}
	if got.Summary.Changes != 3 || got.Summary.Pending != 2 || got.Summary.Verified != 1 ||
		*got.Summary.MeasuredShare != 0.333 {
		t.Fatalf("summary covers every row read: %+v", got.Summary)
	}
	// A part's rows are already cut to the page; its summary covers its
	// window, so the merged summary adds summaries, not rows.
	paged := Result{Rows: aRows, Summary: Summary{Changes: 4, Verified: 1, Pending: 3,
		Measured: 1}}
	if s := Merge(1, paged, b).Summary; s.Changes != 6 || s.Pending != 5 ||
		s.Measured != 1 || *s.MeasuredShare != 0.167 {
		t.Fatalf("paged merge summary = %+v, want 6 changes, 1 measured", s)
	}
	if !got.Truncated || len(got.Errors) != 1 || got.Errors[0].Database != "c" {
		t.Fatalf("truncated %v errors %+v", got.Truncated, got.Errors)
	}
	none := Merge(10)
	if none.Rows == nil || len(none.Rows) != 0 || none.Errors == nil ||
		none.Summary.MeasuredShare != nil {
		t.Fatalf("empty merge = %+v, want [] rows and errors and a null share", none)
	}
}

func TestFilterVerdict(t *testing.T) {
	rows := []Row{{ActionID: 1, Verdict: VerdictImproved}, {ActionID: 2, Verdict: VerdictPending}}
	if got := filterVerdict(rows, ""); len(got) != 2 {
		t.Fatalf("no filter kept %d", len(got))
	}
	got := filterVerdict(rows, VerdictPending)
	if len(got) != 1 || got[0].ActionID != 2 {
		t.Fatalf("pending filter = %+v", got)
	}
	if got := filterVerdict(rows, VerdictExpired); got == nil || len(got) != 0 {
		t.Fatalf("no match = %#v, want empty not nil", got)
	}
}
