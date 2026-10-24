package changereview

import (
	"strings"
	"testing"
	"time"
)

// CHECK-FND-04 (pure part): input_hash identifies one request at one
// commit; content_hash identifies the database-relevant content across
// revisions; receipt_hash seals a decided review.

func TestInputHashNeedsTheSameHeadSHA(t *testing.T) {
	a := validRequest()
	b := validRequest()
	if InputHash("testdb", a) != InputHash("testdb", b) {
		t.Fatal("equal requests hash differently")
	}
	b.PR.HeadSHA = "abc1235"
	if InputHash("testdb", a) == InputHash("testdb", b) {
		t.Fatal("a different head_sha gives the same input_hash")
	}
	c := validRequest()
	c.PR.Title = "Another title"
	c.WaitSeconds = 30
	if InputHash("testdb", a) != InputHash("testdb", c) {
		t.Fatal("the title or wait changed input_hash; only content and commit should")
	}
	for name, edit := range map[string]func(*Request){
		"sql":           func(r *Request) { r.SQL += " " },
		"diff":          func(r *Request) { r.Diff = "+x" },
		"tool":          func(r *Request) { r.MigrationTool = "rails" },
		"transactional": func(r *Request) { r.Transactional = ptr(true) },
		"pr url":        func(r *Request) { r.PR.URL += "1" },
		"file":          func(r *Request) { r.SQLFiles = []SQLFile{{Path: "a.sql", Content: "x"}} },
	} {
		d := validRequest()
		edit(&d)
		if InputHash("testdb", d) == InputHash("testdb", a) {
			t.Errorf("changing the %s kept input_hash", name)
		}
	}
	if InputHash("db1", a) == InputHash("db2", a) {
		t.Fatal("two databases share an input_hash")
	}
	if h := InputHash("testdb", a); len(h) != 64 {
		t.Fatalf("input_hash %q is not hex sha256", h)
	}
}

func TestContentHashTextFallback(t *testing.T) {
	// Not SQL the parser accepts: the text fallback strips comments and
	// collapses whitespace, so formatting edits keep the hash.
	a := []SQLFile{{Path: "a.sql", Content: "FROBNICATE   orders -- why\n WITH care;"}}
	b := []SQLFile{{Path: "b.sql", Content: "/* note */ FROBNICATE orders\tWITH care ;"}}
	if ContentHash(a, "") != ContentHash(b, "") {
		t.Fatal("comment and whitespace edits changed the text-mode content hash")
	}
	c := []SQLFile{{Path: "a.sql", Content: "FROBNICATE orders WITH more care;"}}
	if ContentHash(a, "") == ContentHash(c, "") {
		t.Fatal("an edited statement kept the content hash")
	}
	if ContentHash(a, "hunk 1") == ContentHash(a, "hunk 2") {
		t.Fatal("different DB-relevant hunks kept the content hash")
	}
	quoted := []SQLFile{{Path: "q.sql", Content: "FROBNICATE 'a  -- not a comment';"}}
	quoted2 := []SQLFile{{Path: "q.sql", Content: "FROBNICATE 'a -- not a comment';"}}
	if ContentHash(quoted, "") == ContentHash(quoted2, "") {
		t.Fatal("whitespace inside a string literal was collapsed")
	}
}

func TestContentHashEmpty(t *testing.T) {
	if h := ContentHash(nil, ""); len(h) != 64 {
		t.Fatalf("empty content hash %q", h)
	}
	if ContentHash(nil, "") == ContentHash([]SQLFile{{Content: "SELECT 1"}}, "") {
		t.Fatal("empty content equals one statement")
	}
}

func TestDiffSHA256(t *testing.T) {
	if DiffSHA256("") != "" {
		t.Fatal("an empty diff has a digest")
	}
	if got := DiffSHA256("abc"); got !=
		"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("DiffSHA256(abc) = %s", got)
	}
}

func decidedReview(t *testing.T) Review {
	now := utc(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	r := Review{ID: "8f14e45f-ceea-467a-9575-0c9a1d8e7a01", Database: "testdb",
		Source: SourceCLI, Actor: "mcp:token:ci", InputHash: strings.Repeat("a", 64),
		ContentHash: strings.Repeat("b", 64), State: StateComplete,
		Verdict: VerdictReview, DeterministicVerdict: VerdictReview, ParseMode: ParseAST,
		ModelStatus: ModelNoModel, Statements: []Statement{{Index: 1, Origin: OriginSQLInput,
			SQL: "ALTER TABLE orders ADD COLUMN note text", Kind: "add_column"}},
		Findings:     []Finding{ruleFinding("CR-13", SeverityWarning)},
		Evidence:     []Evidence{mustEvidence(t, "stmt", "1", "s", map[string]any{"n": 1})},
		LandingState: LandingLanded, LandingSource: LandingSourceCatalog,
		LandedLower: ptr(now.Add(-time.Hour)), LandedAt: ptr(now),
		ReceiptVerdict: ReceiptClean, ReceiptDecidedAt: ptr(now.Add(25 * time.Hour)),
		Receipt: &Receipt{Verdict: ReceiptClean, WindowHours: 24,
			Targets: []ReceiptTarget{{QueryID: 42, Verdict: "unchanged", BeforeMeanMs: 1.5,
				AfterMeanMs: 1.4}}},
		ShadowScore: ShadowUnscored, CreditState: CreditNone, CreatedAt: now,
		UpdatedAt: now}
	h, err := ReceiptHash(r)
	if err != nil {
		t.Fatal(err)
	}
	r.ReceiptHash = h
	return r
}

func TestReceiptHashSealsTheDecidedReview(t *testing.T) {
	r := decidedReview(t)
	if !VerifyReceipt(r) {
		t.Fatal("a freshly sealed review does not verify")
	}
	for name, edit := range map[string]func(*Review){
		"finding":   func(r *Review) { r.Findings[0].Severity = SeverityInfo },
		"evidence":  func(r *Review) { r.Evidence[0].Label = "x" },
		"receipt":   func(r *Review) { r.Receipt.Targets[0].AfterMeanMs = 9 },
		"verdict":   func(r *Review) { r.Verdict = VerdictSafe },
		"landed_at": func(r *Review) { r.LandedAt = ptr(r.LandedAt.Add(time.Second)) },
		"statement": func(r *Review) { r.Statements[0].SQL = "DROP TABLE orders" },
		"expect":    func(r *Review) { r.Expectations.WatchQueryIDs = []int64{1} },
		"summary":   func(r *Review) { r.Summary.Critical = 3 },
		"dropped":   func(r *Review) { r.Dropped = []Dropped{{Kind: "claim", Text: "x"}} },
		"not det.":  func(r *Review) { r.NotDetermined = []NotDetermined{{Subject: "x"}} },
	} {
		c := decidedReview(t)
		edit(&c)
		if VerifyReceipt(c) {
			t.Errorf("changing the %s still verifies", name)
		}
	}
	later := decidedReview(t)
	later.DBAVerdict, later.ShadowScore, later.CreditState = DBAConcur, ShadowCorrect,
		CreditFinal
	later.Markdown = "re-rendered"
	if !VerifyReceipt(later) {
		t.Fatal("a DBA verdict, shadow score, credit or comment recorded after the " +
			"receipt broke the seal")
	}
	unsealed := decidedReview(t)
	unsealed.ReceiptHash = ""
	if VerifyReceipt(unsealed) {
		t.Fatal("a review without a receipt hash verifies")
	}
}
