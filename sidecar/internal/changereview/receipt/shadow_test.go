package receipt

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
)

// Shadow auto-clear (CHECK-RCP-08): the eligibility rule is recorded on
// every completed review and scored later, but never decides anything.
// These cases are rejected before any statement is parsed, so they hold
// in a nocgo build too; shadow_cgo_test.go covers the statement kinds.

const gib = int64(1) << 30

// shadowReview is a safe AST review of one CREATE INDEX CONCURRENTLY on a
// 1 GB table, with its table evidence.
func shadowReview(t *testing.T, sql string, tableBytes int64, kind string) changereview.Review {
	t.Helper()
	ev, err := changereview.NewEvidence("table", "rv.rv_orders", "rv_orders",
		map[string]any{"relation": "rv.rv_orders", "kind": kind,
			"total_bytes": tableBytes, "heap_bytes": tableBytes, "reltuples": 200000})
	if err != nil {
		t.Fatal(err)
	}
	return changereview.Review{Verdict: changereview.VerdictSafe,
		ParseMode: changereview.ParseAST,
		Statements: []changereview.Statement{{Index: 1, Origin: changereview.OriginSQLInput,
			SQL: sql, Kind: "create_index", Relations: []string{"rv.rv_orders"},
			EvidenceID: "stmt:1"}},
		Findings: []changereview.Finding{}, Evidence: []changereview.Evidence{ev}}
}

const cic = "CREATE INDEX CONCURRENTLY rv_orders_status_idx ON rv.rv_orders (status)"

func TestEligibleRejectsBeforeParsing(t *testing.T) {
	cases := map[string]struct {
		edit func(*changereview.Review)
		want string
	}{
		"verdict review": {func(r *changereview.Review) {
			r.Verdict = changereview.VerdictReview
		}, "verdict review"},
		"text mode": {func(r *changereview.Review) {
			r.ParseMode = changereview.ParseText
		}, "parse mode text"},
		"CR-17": {func(r *changereview.Review) {
			r.Findings = append(r.Findings, ruleFinding("CR-17"))
		}, "CR-17"},
		"CR-18": {func(r *changereview.Review) {
			r.Findings = append(r.Findings, ruleFinding("CR-18"))
		}, "CR-18"},
		"model-translated DDL": {func(r *changereview.Review) {
			r.Statements = append(r.Statements, changereview.Statement{Index: 2,
				Origin: changereview.OriginModelTranslated, SQL: cic, Kind: "create_index"})
		}, "model-translated"},
		"CR-19 finding": {func(r *changereview.Review) {
			r.Findings = append(r.Findings, ruleFinding("CR-19"))
		}, "CR-19"},
		"model finding": {func(r *changereview.Review) {
			r.Findings = append(r.Findings, changereview.Finding{Kind: "fix",
				Severity: changereview.SeverityWarning, Origin: changereview.OriginModel})
		}, "model finding"},
		"no statements": {func(r *changereview.Review) {
			r.Statements = nil
		}, "no statements"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := shadowReview(t, cic, gib, "table")
			tc.edit(&r)
			ok, why := Eligible(r, 10*gib)
			if ok || !strings.Contains(why, tc.want) {
				t.Fatalf("Eligible = %v (%q), want false naming %q", ok, why, tc.want)
			}
		})
	}
}

func ruleFinding(rule string) changereview.Finding {
	return changereview.Finding{RuleID: rule, Kind: "k", Severity: changereview.SeverityInfo,
		Origin: changereview.OriginRule, Title: rule}
}

func TestScore(t *testing.T) {
	cases := []struct {
		dba, receipt, want string
	}{
		{changereview.DBAConcur, changereview.ReceiptClean, changereview.ShadowCorrect},
		{changereview.DBAConcur, changereview.ReceiptCleanLowTraffic,
			changereview.ShadowCorrect},
		{changereview.DBAOverride, changereview.ReceiptClean, changereview.ShadowIncorrect},
		{changereview.DBAOverride, changereview.ReceiptPending, changereview.ShadowIncorrect},
		{changereview.DBAConcurWithEdits, "", changereview.ShadowIncorrect},
		{changereview.DBAConcur, changereview.ReceiptRegressed, changereview.ShadowIncorrect},
		{"", changereview.ReceiptRegressed, changereview.ShadowIncorrect},
		{changereview.DBAConcur, changereview.ReceiptPending, changereview.ShadowUnscored},
		{"", changereview.ReceiptClean, changereview.ShadowUnscored},
		{changereview.DBAConcur, changereview.ReceiptUnverifiable,
			changereview.ShadowUnscored},
		{"", "", changereview.ShadowUnscored},
	}
	for _, c := range cases {
		if got := Score(c.dba, c.receipt); got != c.want {
			t.Errorf("Score(%q, %q) = %q, want %q", c.dba, c.receipt, got, c.want)
		}
	}
}

// AutoClear refuses a risky review before parsing anything. Whether it
// binds the configured table-size limit needs the parser to accept the
// statement first: shadow_cgo_test.go checks it with a positive control.
func TestAutoClearRefusesARiskyReview(t *testing.T) {
	r := shadowReview(t, cic, gib, "table")
	r.Verdict = changereview.VerdictRisky
	if AutoClear(config.DefaultChangeReviewConfig())(r) {
		t.Fatal("a risky review was eligible")
	}
}
