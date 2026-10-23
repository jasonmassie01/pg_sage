package notify

import (
	"strings"
	"testing"
)

// change_review_needs_dba (CHECK-SRF-07): emitted once when a Change
// Review completes with verdict review or risky and no DBA verdict. It
// names the database, the PR (scheme and host only: notify.RedactURL),
// the verdict and the top finding, and links the review in the UI.

func crInfo() ChangeReviewInfo {
	return ChangeReviewInfo{ID: "3f0c8a52-0000-4000-8000-000000000001", Database: "orders",
		PRURL: "https://github.com/acme/shop/pull/42?token=s3cret", PRTitle: "Rename email",
		Verdict: "risky", TopRule: "CR-03", TopFinding: "email is still read by 2 statements",
		Critical: 1, Warning: 2, Link: "https://sage.example.com/#/reviews?id=3f0c8a52"}
}

func TestChangeReviewNeedsDBAEvent(t *testing.T) {
	evt := ChangeReviewNeedsDBAEvent(crInfo())
	if evt.Type != "change_review_needs_dba" || evt.Severity != "warning" {
		t.Fatalf("type/severity %q/%q", evt.Type, evt.Severity)
	}
	if evt.DedupKey != "change_review:3f0c8a52-0000-4000-8000-000000000001" {
		t.Fatalf("dedup key %q, want one per review", evt.DedupKey)
	}
	for _, want := range []string{"orders", "risky", "Rename email"} {
		if !strings.Contains(evt.Subject, want) {
			t.Errorf("subject %q lacks %q", evt.Subject, want)
		}
	}
	for _, want := range []string{"Database: orders", "Verdict: risky",
		"PR: https://github.com/[redacted]", "CR-03: email is still read by 2 statements",
		"1 critical, 2 warning", "https://sage.example.com/#/reviews?id=3f0c8a52"} {
		if !strings.Contains(evt.Body, want) {
			t.Errorf("body %q lacks %q", evt.Body, want)
		}
	}
	all := evt.Subject + evt.Body
	for _, leak := range []string{"s3cret", "acme/shop/pull/42"} {
		if strings.Contains(all, leak) {
			t.Errorf("the PR link leaked %q: %s", leak, all)
		}
	}
	data := evt.Data
	want := map[string]any{"review_id": "3f0c8a52-0000-4000-8000-000000000001",
		"database": "orders", "pr_url": "https://github.com/[redacted]", "verdict": "risky",
		"top_rule": "CR-03", "top_finding": "email is still read by 2 statements",
		"critical": 1, "warning": 2,
		"link": "https://sage.example.com/#/reviews?id=3f0c8a52"}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("data[%s] = %#v, want %#v", k, data[k], v)
		}
	}
}

func TestChangeReviewNeedsDBAEventWithoutAPROrFinding(t *testing.T) {
	info := ChangeReviewInfo{ID: "id-1", Database: "orders", Verdict: "review"}
	evt := ChangeReviewNeedsDBAEvent(info)
	if !strings.Contains(evt.Body, "PR: none") || strings.Contains(evt.Body, "Top finding") {
		t.Fatalf("body without PR or finding: %q", evt.Body)
	}
	if evt.Data["pr_url"] != "" || evt.Data["top_finding"] != "" {
		t.Fatalf("empty fields must stay empty: %#v", evt.Data)
	}
	if !strings.Contains(evt.Subject, "orders") || !strings.Contains(evt.Subject, "review") {
		t.Fatalf("subject %q", evt.Subject)
	}
}

func TestChangeReviewNeedsDBAEventRedactsURLsInFreeText(t *testing.T) {
	info := crInfo()
	info.PRTitle = "see https://hooks.slack.com/services/T0/B0/xyz for context"
	info.TopFinding = "read by https://internal.example/path?key=abc"
	evt := ChangeReviewNeedsDBAEvent(info)
	for _, leak := range []string{"T0/B0/xyz", "key=abc"} {
		if strings.Contains(evt.Subject+evt.Body, leak) {
			t.Fatalf("free text leaked %q: %s %s", leak, evt.Subject, evt.Body)
		}
	}
	long := crInfo()
	long.TopFinding = strings.Repeat("x", 2000)
	if got := ChangeReviewNeedsDBAEvent(long).Body; len([]rune(got)) > 1200 {
		t.Fatalf("body of %d runes, want the finding truncated", len([]rune(got)))
	}
}

func TestChangeReviewNeedsDBAIsARegisteredEvent(t *testing.T) {
	if !ValidEventTypes["change_review_needs_dba"] {
		t.Fatal("change_review_needs_dba must be a valid rule event")
	}
	if got := DefaultMinSeverity("change_review_needs_dba"); got != "warning" {
		t.Fatalf("default min severity %q, want warning (the event's own)", got)
	}
	if sev, ok := maxEventSeverity("change_review_needs_dba"); !ok || sev != "warning" {
		t.Fatalf("max severity %q %v", sev, ok)
	}
}
