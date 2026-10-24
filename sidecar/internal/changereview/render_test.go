package changereview

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// CHECK-FND-08: renderer golden files for six fixtures, the marker on
// line 1, evidence ids at the end of every finding line, model
// suggestions fenced and labeled, and the 65,000-character cap with a
// truncation note that links the UI.

var updateGolden = flag.Bool("update", false, "rewrite the renderer golden files")

const renderID = "8f14e45f-ceea-467a-9575-0c9a1d8e7a01"

var renderAt = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func renderBase(t *testing.T) Review {
	return Review{ID: renderID, Database: "shop", Source: SourceCLI, Actor: "mcp:token:ci",
		PRURL: "https://github.com/acme/shop/pull/7", PRNumber: 7, PRTitle: "Rename email",
		HeadSHA: "abc1234", State: StateComplete, ParseMode: ParseAST, ModelStatus: ModelOK,
		Statements: []Statement{{Index: 1, Origin: OriginSQLInput,
			File: "db/migrate/001.sql", Line: 1, Kind: "add_column",
			SQL: "ALTER TABLE public.orders ADD COLUMN note text", EvidenceID: "stmt:1",
			Relations: []string{"public.orders"}}},
		Evidence: []Evidence{mustEvidence(t, "stmt", "1", "statement 1",
			map[string]any{"rows": 200000})},
		Expectations: Expectations{PostConditions: []PostCondition{{StatementIndex: 1,
			Kind: PostColumnPresent, Relation: "public.orders", Column: "note"}},
			WatchQueryIDs: []int64{-42, 7}, Relations: []string{"public.orders"}},
		Summary: ReviewSummary{Calibration: &Calibration{Reviews: 12, FalseSafe: 0,
			ByVerdict: map[string]VerdictClass{VerdictSafe: {Reviews: 8, Decided: 6,
				Concurred: 6, Receipts: 5, Clean: 5}}}},
		LandingState: LandingAwaiting, ReceiptVerdict: ReceiptPending,
		ShadowScore: ShadowUnscored, CreditState: CreditNone, CreatedAt: renderAt,
		CompletedAt: ptr(renderAt.Add(40 * time.Second)), UpdatedAt: renderAt}
}

func renderFixtures(t *testing.T) map[string]Review {
	safe := renderBase(t)
	safe.Verdict, safe.DeterministicVerdict = VerdictSafe, VerdictSafe
	safe.Findings = []Finding{ruleFinding("CR-15", SeverityInfo)}

	review := renderBase(t)
	review.Verdict, review.DeterministicVerdict = VerdictReview, VerdictReview
	review.Findings = []Finding{ruleFinding("CR-13", SeverityWarning, "stmt:1", "table:pub"),
		ruleFinding("CR-15", SeverityInfo)}

	risky := renderBase(t)
	risky.Verdict, risky.DeterministicVerdict = VerdictRisky, VerdictRisky
	risky.Findings = []Finding{{RuleID: "CR-03", Kind: "column_still_referenced",
		Severity: SeverityCritical, Origin: OriginRule,
		Title:       "orders.email is still read by 1 hot statement",
		Text:        "queryid -42 ran 2000 times this week <script>alert(1)</script>\nsecond line",
		EvidenceIDs: []string{"stmt:1", "queryid:-42"}},
		ruleFinding("CR-02", SeverityWarning)}

	off := renderBase(t)
	off.Verdict, off.DeterministicVerdict, off.ModelStatus = VerdictReview, VerdictReview,
		ModelNoModel
	off.Findings = []Finding{ruleFinding("CR-20", SeverityWarning)}
	off.NotDetermined = []NotDetermined{{Subject: "app-code changes",
		Reason: "no model is configured"}}

	return map[string]Review{"safe": safe, "review": review, "risky": risky,
		"llm_off": off, "model_suggestions": modelFixture(t),
		"decided_receipt": decidedFixture(t)}
}

func modelFixture(t *testing.T) Review {
	model := renderBase(t)
	model.Verdict, model.DeterministicVerdict = VerdictReview, VerdictSafe
	model.Findings = []Finding{{Kind: "n_plus_one", Severity: SeverityWarning,
		Origin: OriginModel, Title: "Order.where in a loop", Text: "50 queries per request",
		EvidenceIDs: []string{"stmt:2"}, Suggestion: &Suggestion{Language: "ruby",
			Code: "orders = Order.where(id: ids).to_a\n# ``` fence inside code"}}}
	model.Statements = append(model.Statements, Statement{Index: 2,
		Origin: OriginModelExtracted, File: "app/controllers/orders_controller.rb", Line: 12,
		SQL: "SELECT * FROM orders WHERE id = $1", Kind: "select", EvidenceID: "stmt:2",
		Confidence: 0.8, ORM: "activerecord", InLoop: true})
	model.Dropped = []Dropped{{Kind: "finding", Text: "uncited", Reason: "uncited"},
		{Kind: "claim", Text: "made-up 99%", Reason: "ungrounded"}}
	model.Tokens = 4200
	return model
}

func decidedFixture(t *testing.T) Review {
	decided := renderBase(t)
	decided.Verdict, decided.DeterministicVerdict = VerdictSafe, VerdictSafe
	decided.DBAVerdict, decided.DBAActor = DBAConcur, "user:1"
	decided.LandingState, decided.LandingSource = LandingLanded, LandingSourceCatalog
	decided.LandedLower = ptr(renderAt.Add(time.Hour))
	decided.LandedAt = ptr(renderAt.Add(2 * time.Hour))
	decided.ReceiptVerdict = ReceiptClean
	decided.ReceiptDecidedAt = ptr(renderAt.Add(27 * time.Hour))
	decided.Receipt = &Receipt{Verdict: ReceiptClean, WindowHours: 24, Tolerance: "met",
		Targets: []ReceiptTarget{{QueryID: -42, Verdict: "unchanged", BeforeMeanMs: 1.25,
			AfterMeanMs: 1.2, BeforeCalls: 900, AfterCalls: 950, ChangePct: -4}},
		BornQueryIDs: []BornQuery{{QueryID: 77, FirstSeen: renderAt.Add(3 * time.Hour),
			MeanMs: 0.4, Calls: 31}}}
	decided.ReceiptHash = strings.Repeat("9", 64)
	decided.CreditState, decided.CreditedMinutes = CreditFinal, 15
	return decided
}

func TestRenderGoldens(t *testing.T) {
	opts := RenderOptions{UIBaseURL: "https://sage.example.com"}
	fixtures := renderFixtures(t)
	if len(fixtures) != 6 {
		t.Fatalf("%d fixtures, want 6", len(fixtures))
	}
	for name, r := range fixtures {
		t.Run(name, func(t *testing.T) {
			got := RenderMarkdown(r, opts)
			path := filepath.Join("testdata", "render", name+".md")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update): %v", err)
			}
			if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
				t.Fatalf("%s drifted from %s:\n%s", name, path, got)
			}
			assertRenderContract(t, r, got)
		})
	}
}

func assertRenderContract(t *testing.T, r Review, md string) {
	t.Helper()
	lines := strings.Split(md, "\n")
	receipt := r.ReceiptHash
	if receipt == "" {
		receipt = "pending"
	}
	want := "<!-- pg_sage-review db=" + r.Database + " id=" + r.ID + " receipt=" + receipt +
		" -->"
	if lines[0] != want {
		t.Fatalf("line 1 = %q, want %q", lines[0], want)
	}
	for _, f := range r.Findings {
		line := findLine(lines, escapeMarkdown(f.Title))
		if line == "" {
			t.Fatalf("no line renders finding %q", f.Title)
		}
		if !strings.HasSuffix(line, "(evidence: "+strings.Join(f.EvidenceIDs, ", ")+")") {
			t.Fatalf("finding line %q does not end with its evidence ids", line)
		}
		if f.Suggestion != nil {
			assertFencedSuggestion(t, md, f.Suggestion.Code)
		}
	}
	if strings.Contains(md, "<script>") {
		t.Fatal("untrusted text rendered as HTML")
	}
	if strings.Count(md, "<!-- pg_sage-review") != 1 {
		t.Fatal("the marker appears more than once")
	}
}

func findLine(lines []string, needle string) string {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

func assertFencedSuggestion(t *testing.T, md, code string) {
	t.Helper()
	label := "suggested by pg_sage's model — verify"
	i := strings.Index(md, label)
	if i < 0 {
		t.Fatalf("no %q label", label)
	}
	rest := md[i+len(label):]
	open := strings.Index(rest, "````")
	body := strings.Index(rest, strings.Split(code, "\n")[0])
	if open < 0 || body < open {
		t.Fatalf("the suggestion is not inside a fence after its label:\n%s", rest)
	}
	closing := strings.Index(rest[body:], "\n````")
	if closing < 0 {
		t.Fatal("the suggestion fence is not closed with a fence longer than its content")
	}
}

func TestRenderTruncatesAt65000(t *testing.T) {
	r := renderBase(t)
	r.Verdict, r.DeterministicVerdict = VerdictRisky, VerdictRisky
	for i := 0; i < 400; i++ {
		f := ruleFinding("CR-17", SeverityWarning, "stmt:1")
		f.Title = "finding " + strconv.Itoa(i) + " " + strings.Repeat("x", 120)
		f.Text = strings.Repeat("long explanation ", 20)
		r.Findings = append(r.Findings, f)
	}
	md := RenderMarkdown(r, RenderOptions{UIBaseURL: "https://sage.example.com/"})
	if n := len([]rune(md)); n > MaxCommentRunes {
		t.Fatalf("rendered %d characters, more than %d", n, MaxCommentRunes)
	}
	link := "https://sage.example.com/#/reviews?id=" + renderID + "&database=shop"
	if !strings.Contains(md, "not shown") || !strings.Contains(md, link) {
		t.Fatalf("no truncation note linking %s:\n%s", link, md[len(md)-400:])
	}
	if !strings.HasPrefix(md, "<!-- pg_sage-review db=shop") {
		t.Fatal("truncation lost the marker")
	}
	small := RenderMarkdown(r, RenderOptions{MaxRunes: 2000})
	// Without the UI's address the note names the review (a relative link
	// would open nothing from GitHub).
	if n := len([]rune(small)); n > 2000 || !strings.Contains(small, "review "+renderID) {
		t.Fatalf("a 2000-rune cap rendered %d runes", n)
	}
}

func TestRenderEmptyAndRunning(t *testing.T) {
	md := RenderMarkdown(Review{ID: renderID, Database: "shop", State: StateRunning},
		RenderOptions{})
	if !strings.HasPrefix(md, "<!-- pg_sage-review db=shop id="+renderID+" receipt=pending -->") ||
		!strings.Contains(md, "running") {
		t.Fatalf("running review renders %q", md)
	}
	failed := RenderMarkdown(Review{ID: renderID, Database: "shop", State: StateFailed,
		FailureReason: "deterministic stage timed out"}, RenderOptions{})
	if !strings.Contains(failed, "deterministic stage timed out") {
		t.Fatalf("failed review renders %q", failed)
	}
}

func TestRenderJSON(t *testing.T) {
	r := renderFixtures(t)["model_suggestions"]
	raw, err := RenderJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if out["marker"] != Marker(r) || out["verdict"] != VerdictReview || out["id"] != renderID {
		t.Fatalf("json = %v", out)
	}
	if _, ok := out["transcript"]; ok {
		t.Fatal("the JSON comment carries the transcript")
	}
	if fs, _ := out["findings"].([]any); len(fs) != 1 {
		t.Fatalf("findings = %v", out["findings"])
	}
}

func TestMarkerSanitizesDatabaseName(t *testing.T) {
	m := Marker(Review{ID: renderID, Database: "evil --> <b>x</b>"})
	if strings.Count(m, "-->") != 1 || strings.Contains(m, "<b>") {
		t.Fatalf("marker %q can be broken out of", m)
	}
}
