package cases

import (
	"strings"
	"testing"
	"time"
)

// Change Review cases (CHECK-UI-04, the pure part): a complete review with
// verdict review or risky and no DBA verdict is a DBA work item. Risky is
// critical, review is warning, the top finding is the why, why_now counts
// the wait in whole hours, and there are never action candidates: the
// only next step is a DBA verdict. Safe, running, failed and decided
// reviews are not cases.
//
// No concurrent access tests: ProjectChangeReview is a pure function of
// its arguments and holds no shared state.

var crNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func crSource(verdict string) SourceChangeReview {
	return SourceChangeReview{
		ID:           "7f8a1c2e-0000-4000-8000-000000000001",
		DatabaseName: "orders",
		Repo:         "acme/billing",
		PRURL:        "https://github.com/acme/billing/pull/42",
		PRNumber:     42,
		PRTitle:      "Rename orders.email",
		State:        "complete",
		Verdict:      verdict,
		CompletedAt:  crNow.Add(-3*time.Hour - 20*time.Minute),
		Findings: []ChangeReviewFinding{
			{RuleID: "CR-15", Severity: "info", Title: "int4 primary key"},
			{RuleID: "CR-01", Severity: "warning", Title: "ACCESS EXCLUSIVE rewrite"},
			{RuleID: "CR-03", Severity: "critical", Title: "email still read by 1 query"},
			{RuleID: "CR-02", Severity: "critical", Title: "lock queue at peak"},
		},
	}
}

func TestProjectChangeReview_RiskyIsCriticalWithTheTopFinding(t *testing.T) {
	c, ok := ProjectChangeReview(crSource("risky"), crNow)
	if !ok {
		t.Fatal("a risky review waiting for a DBA must be a case")
	}
	if SourceChangeReviewType != "change_review" || c.SourceType != SourceChangeReviewType {
		t.Fatalf("source type %q, want change_review", c.SourceType)
	}
	wantKey := "change_review:7f8a1c2e-0000-4000-8000-000000000001"
	if c.IdentityKey != wantKey || c.ID != wantKey {
		t.Fatalf("identity %q / id %q, want %q", c.IdentityKey, c.ID, wantKey)
	}
	if len(c.SourceIDs) != 1 || c.SourceIDs[0] != "7f8a1c2e-0000-4000-8000-000000000001" {
		t.Fatalf("source ids %v", c.SourceIDs)
	}
	if c.Severity != SeverityCritical || c.State != StateOpen {
		t.Fatalf("severity %q state %q, want critical/open", c.Severity, c.State)
	}
	// The first critical finding in stored order is the top one.
	if c.Why != "CR-03 email still read by 1 query" {
		t.Fatalf("why %q, want the first critical finding", c.Why)
	}
	if c.WhyNow != "waiting 3h for a DBA verdict" {
		t.Fatalf("why_now %q", c.WhyNow)
	}
	if c.DatabaseName != "orders" {
		t.Fatalf("database %q", c.DatabaseName)
	}
	if !strings.Contains(c.Title, "Rename orders.email") ||
		!strings.Contains(c.Title, "risky") {
		t.Fatalf("title %q must name the PR and the verdict", c.Title)
	}
	if len(c.ActionCandidates) != 0 || len(c.Actions) != 0 {
		t.Fatalf("a change review case has no action candidates: %+v", c.ActionCandidates)
	}
	if !c.ObservedAt.Equal(crSource("risky").CompletedAt) {
		t.Fatalf("observed at %v, want the completion time", c.ObservedAt)
	}
}

func TestProjectChangeReview_EvidenceLinksTheReview(t *testing.T) {
	c, _ := ProjectChangeReview(crSource("risky"), crNow)
	if len(c.Evidence) != 1 || c.Evidence[0].Type != "change_review" {
		t.Fatalf("evidence %+v, want one change_review item", c.Evidence)
	}
	d := c.Evidence[0].Detail
	if d["review_id"] != "7f8a1c2e-0000-4000-8000-000000000001" || d["verdict"] != "risky" ||
		d["pr_url"] != "https://github.com/acme/billing/pull/42" || d["top_rule"] != "CR-03" {
		t.Fatalf("detail %+v", d)
	}
	if d["critical"] != 2 || d["warning"] != 1 {
		t.Fatalf("counts critical=%v warning=%v, want 2 and 1", d["critical"], d["warning"])
	}
	wantLink := "#/reviews?id=7f8a1c2e-0000-4000-8000-000000000001&database=orders"
	if d["link"] != wantLink {
		t.Fatalf("link %v, want %s", d["link"], wantLink)
	}
	if !strings.Contains(c.Evidence[0].Summary, "2 critical") ||
		!strings.Contains(c.Evidence[0].Summary, "1 warning") {
		t.Fatalf("summary %q", c.Evidence[0].Summary)
	}
}

func TestProjectChangeReview_ReviewIsWarning(t *testing.T) {
	src := crSource("review")
	src.Findings = src.Findings[:2] // info, warning
	c, ok := ProjectChangeReview(src, crNow)
	if !ok || c.Severity != SeverityWarning {
		t.Fatalf("ok=%v severity %q, want a warning case", ok, c.Severity)
	}
	if c.Why != "CR-01 ACCESS EXCLUSIVE rewrite" {
		t.Fatalf("why %q, want the warning finding over the info one", c.Why)
	}
}

func TestProjectChangeReview_NotACase(t *testing.T) {
	cases := map[string]func(*SourceChangeReview){
		"safe":           func(s *SourceChangeReview) { s.Verdict = "safe" },
		"unknown":        func(s *SourceChangeReview) { s.Verdict = "unsafe" },
		"empty verdict":  func(s *SourceChangeReview) { s.Verdict = "" },
		"running":        func(s *SourceChangeReview) { s.State = "running" },
		"failed":         func(s *SourceChangeReview) { s.State = "failed" },
		"concurred":      func(s *SourceChangeReview) { s.DBAVerdict = "concur" },
		"overridden":     func(s *SourceChangeReview) { s.DBAVerdict = "override" },
		"with edits":     func(s *SourceChangeReview) { s.DBAVerdict = "concur_with_edits" },
		"no id":          func(s *SourceChangeReview) { s.ID = "" },
		"zero completed": func(s *SourceChangeReview) { s.CompletedAt = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			src := crSource("risky")
			mutate(&src)
			if c, ok := ProjectChangeReview(src, crNow); ok {
				t.Fatalf("projected %+v, want no case", c)
			}
		})
	}
}

func TestProjectChangeReview_WaitHoursBoundaries(t *testing.T) {
	tests := []struct {
		wait time.Duration
		want string
	}{
		{0, "waiting 0h for a DBA verdict"},
		{59*time.Minute + 59*time.Second, "waiting 0h for a DBA verdict"},
		{time.Hour, "waiting 1h for a DBA verdict"},
		{25*time.Hour + 30*time.Minute, "waiting 25h for a DBA verdict"},
		// A completion stamped after the reader's clock (clock skew) is
		// never a negative wait.
		{-2 * time.Hour, "waiting 0h for a DBA verdict"},
	}
	for _, tc := range tests {
		src := crSource("review")
		src.CompletedAt = crNow.Add(-tc.wait)
		c, ok := ProjectChangeReview(src, crNow)
		if !ok || c.WhyNow != tc.want {
			t.Errorf("wait %v: ok=%v why_now %q, want %q", tc.wait, ok, c.WhyNow, tc.want)
		}
	}
}

func TestProjectChangeReview_TitleFallsBackToURLThenID(t *testing.T) {
	src := crSource("review")
	src.PRTitle = ""
	c, _ := ProjectChangeReview(src, crNow)
	if !strings.Contains(c.Title, "acme/billing#42") {
		t.Fatalf("title %q, want the repo and PR number without a title", c.Title)
	}
	src.Repo, src.PRNumber = "", 0
	c, _ = ProjectChangeReview(src, crNow)
	if !strings.Contains(c.Title, "https://github.com/acme/billing/pull/42") {
		t.Fatalf("title %q, want the PR URL", c.Title)
	}
	src.PRURL = ""
	c, _ = ProjectChangeReview(src, crNow)
	if !strings.Contains(c.Title, "7f8a1c2e") {
		t.Fatalf("title %q, want the short review id", c.Title)
	}
}

func TestProjectChangeReview_NoFindingsStillExplainsTheVerdict(t *testing.T) {
	src := crSource("review")
	src.Findings = nil
	c, ok := ProjectChangeReview(src, crNow)
	if !ok || c.Why == "" || !strings.Contains(c.Why, "review") {
		t.Fatalf("ok=%v why %q, want a why naming the verdict", ok, c.Why)
	}
	if c.Evidence[0].Detail["top_rule"] != "" {
		t.Fatalf("top_rule %v, want empty without findings", c.Evidence[0].Detail["top_rule"])
	}
}

func TestChangeReviewLinkEscapesTheDatabase(t *testing.T) {
	got := ChangeReviewLink("db one&x", "abc")
	if got != "#/reviews?id=abc&database=db+one%26x" {
		t.Fatalf("link %q", got)
	}
	if ChangeReviewIdentityKey("abc") != "change_review:abc" {
		t.Fatalf("identity %q", ChangeReviewIdentityKey("abc"))
	}
}
