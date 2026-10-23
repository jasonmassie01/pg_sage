package notify

import "fmt"

// EventChangeReviewNeedsDBA is emitted once when a Change Review completes
// with verdict review or risky and no DBA verdict carried over.
const EventChangeReviewNeedsDBA = "change_review_needs_dba"

const maxChangeReviewTextRunes = 300

// ChangeReviewInfo is the review payload of change_review_needs_dba. It is
// a plain struct so notify stays independent of the changereview package.
type ChangeReviewInfo struct {
	ID         string
	Database   string
	PRURL      string
	PRTitle    string
	Verdict    string
	TopRule    string // rule id of the top finding ("" for a model finding)
	TopFinding string // title of the most severe finding
	Critical   int
	Warning    int
	Link       string // the review in the pg_sage UI
}

// ChangeReviewNeedsDBAEvent asks a DBA for a verdict on a review. The PR
// link keeps only its scheme and host (RedactURL); URLs in the PR title and
// finding text are redacted the same way. The event has one severity
// (warning) so a rule at its default min_severity always fires; the
// verdict is in the subject and the data.
func ChangeReviewNeedsDBAEvent(info ChangeReviewInfo) Event {
	pr := ""
	if info.PRURL != "" {
		pr = RedactURL(info.PRURL)
	}
	title := TruncateRunes(RedactURLs(info.PRTitle), maxChangeReviewTextRunes)
	top := TruncateRunes(RedactURLs(info.TopFinding), maxChangeReviewTextRunes)
	subject := fmt.Sprintf("Change review needs a DBA: %s on %s", info.Verdict,
		info.Database)
	if title != "" {
		subject += ": " + title
	}
	body := fmt.Sprintf("Database: %s\nVerdict: %s (%d critical, %d warning)\nPR: %s",
		info.Database, info.Verdict, info.Critical, info.Warning, orNone(pr))
	if top != "" {
		lead := top
		if info.TopRule != "" {
			lead = info.TopRule + ": " + top
		}
		body += "\nTop finding: " + lead
	}
	if info.Link != "" {
		body += "\nReview: " + info.Link
	}
	return Event{Type: EventChangeReviewNeedsDBA, Severity: "warning", Subject: subject,
		Body: body, DedupKey: "change_review:" + info.ID,
		Data: map[string]any{"review_id": info.ID, "database": info.Database,
			"pr_url": pr, "pr_title": title, "verdict": info.Verdict,
			"top_rule": info.TopRule, "top_finding": top, "critical": info.Critical,
			"warning": info.Warning, "link": info.Link}}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
