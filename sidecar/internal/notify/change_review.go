package notify

import (
	"fmt"
	"net/url"
	"regexp"
)

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
// link keeps its pull request path so the DBA can open it, and nothing
// else (PRLink); URLs in the PR title and finding text are redacted to
// their host. Without the UI's address the body names the review. The event has one severity
// (warning) so a rule at its default min_severity always fires; the
// verdict is in the subject and the data.
func ChangeReviewNeedsDBAEvent(info ChangeReviewInfo) Event {
	pr := ""
	if info.PRURL != "" {
		pr = PRLink(info.PRURL)
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
	} else {
		body += "\nReview: " + info.ID + " on pg_sage's Reviews page (set " +
			"change_review.ui_base_url to link it)"
	}
	return Event{Type: EventChangeReviewNeedsDBA, Severity: "warning", Subject: subject,
		Body: body, DedupKey: "change_review:" + info.ID,
		Data: map[string]any{"review_id": info.ID, "database": info.Database,
			"pr_url": pr, "pr_title": title, "verdict": info.Verdict,
			"top_rule": info.TopRule, "top_finding": top, "critical": info.Critical,
			"warning": info.Warning, "link": info.Link}}
}

// prPath is a pull request's path on GitHub (/owner/repo/pull/N), GitLab
// (/group/.../project/-/merge_requests/N) or Bitbucket
// (/workspace/repo/pull-requests/N): names and a number, no secret.
var prPath = regexp.MustCompile(`^(?:/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+/pull/\d+|` +
	`(?:/[A-Za-z0-9._-]+)+/-/merge_requests/\d+|` +
	`/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+/pull-requests/\d+)$`)

// PRLink is a pull request URL a DBA can open: https, its host and its
// pull request path, without user info, query or fragment. Anything else
// is redacted to its host (RedactURL).
func PRLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || !prPath.MatchString(u.Path) {
		return RedactURL(raw)
	}
	return "https://" + u.Host + u.Path
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
