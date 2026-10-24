package cases

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// SourceChangeReviewType is a Change Review waiting for a DBA verdict: a
// complete review with verdict review or risky and no DBA verdict yet.
const SourceChangeReviewType SourceType = "change_review"

// ChangeReviewFinding is the part of a review finding a case shows.
type ChangeReviewFinding struct {
	RuleID   string
	Severity string
	Title    string
}

// SourceChangeReview is one sage.change_review row as the cases view
// reads it. Findings are in stored order.
type SourceChangeReview struct {
	ID           string
	DatabaseName string
	Repo         string
	PRURL        string
	PRNumber     int
	PRTitle      string
	State        string
	Verdict      string
	DBAVerdict   string
	CompletedAt  time.Time
	Findings     []ChangeReviewFinding
}

// ChangeReviewIdentityKey is the case identity of review id.
func ChangeReviewIdentityKey(id string) string { return "change_review:" + id }

// ChangeReviewLink is the UI route of a review ("#/reviews?id=&database=").
func ChangeReviewLink(database, id string) string {
	return "#/reviews?id=" + url.QueryEscape(id) + "&database=" + url.QueryEscape(database)
}

// ProjectChangeReview is the DBA work item of a review, or false when the
// review needs no DBA verdict: not complete, safe, already decided, or
// without an id or completion time. Risky is critical and review is
// warning; why is the top finding and why_now the whole hours waited.
// There are no action candidates: the next step is the DBA's verdict.
func ProjectChangeReview(r SourceChangeReview, now time.Time) (Case, bool) {
	severity, ok := changeReviewSeverity(r.Verdict)
	if !ok || r.State != "complete" || r.DBAVerdict != "" || r.ID == "" ||
		r.CompletedAt.IsZero() {
		return Case{}, false
	}
	top, critical, warning := topChangeReviewFinding(r.Findings)
	why := "verdict " + r.Verdict
	if top != nil {
		why = strings.TrimSpace(top.RuleID + " " + top.Title)
	}
	topRule := ""
	if top != nil {
		topRule = top.RuleID
	}
	return NewCase(CaseInput{
		SourceType:   SourceChangeReviewType,
		SourceID:     r.ID,
		DatabaseName: r.DatabaseName,
		IdentityKey:  ChangeReviewIdentityKey(r.ID),
		Title:        fmt.Sprintf("Change review %s: %s", r.Verdict, changeReviewLabel(r)),
		Severity:     severity,
		Why:          why,
		WhyNow:       fmt.Sprintf("waiting %dh for a DBA verdict", waitedHours(r, now)),
		Evidence: []Evidence{{
			Type: "change_review",
			Summary: fmt.Sprintf("%s review: %d critical, %d warning", r.Verdict,
				critical, warning),
			Detail: map[string]any{
				"review_id": r.ID, "verdict": r.Verdict, "pr_url": r.PRURL,
				"repo": r.Repo, "pr_number": r.PRNumber, "critical": critical,
				"warning": warning, "top_rule": topRule,
				"link": ChangeReviewLink(r.DatabaseName, r.ID),
			},
		}},
		ObservedAt: r.CompletedAt,
	}), true
}

func changeReviewSeverity(verdict string) (Severity, bool) {
	switch verdict {
	case "risky":
		return SeverityCritical, true
	case "review":
		return SeverityWarning, true
	}
	return "", false
}

// topChangeReviewFinding is the first critical finding, else the first
// warning, else the first finding; with the critical and warning counts.
func topChangeReviewFinding(fs []ChangeReviewFinding) (*ChangeReviewFinding, int, int) {
	var top *ChangeReviewFinding
	rank := 3
	critical, warning := 0, 0
	for i := range fs {
		r := 2
		switch fs[i].Severity {
		case "critical":
			critical++
			r = 0
		case "warning":
			warning++
			r = 1
		}
		if r < rank {
			top, rank = &fs[i], r
		}
	}
	return top, critical, warning
}

// changeReviewLabel names the PR: its title, repo#number, URL, or the
// short review id.
func changeReviewLabel(r SourceChangeReview) string {
	switch {
	case strings.TrimSpace(r.PRTitle) != "":
		return r.PRTitle
	case r.Repo != "" && r.PRNumber > 0:
		return fmt.Sprintf("%s#%d", r.Repo, r.PRNumber)
	case r.PRURL != "":
		return r.PRURL
	}
	id := r.ID
	if len(id) > 8 {
		id = id[:8]
	}
	return "review " + id
}

// waitedHours is the whole hours since completion, never negative.
func waitedHours(r SourceChangeReview, now time.Time) int {
	d := now.Sub(r.CompletedAt)
	if d < 0 {
		return 0
	}
	return int(d / time.Hour)
}
