package notify

import (
	"fmt"
	"strings"
	"time"
)

// FactExpiry is a confirmed fact that expired (facts-08): it no longer
// binds what pg_sage does. MissingSince is when its subject was first
// found absent (nil for an expiry by date); RestoreDays, when positive, is
// how long after the expiry the same subject and value restore it.
type FactExpiry struct {
	Database     string
	FactID       int64
	Title        string
	ConfirmedBy  string
	Reason       string
	MissingSince *time.Time
	RestoreDays  int
}

// FactExpiredEvent tells operators that a confirmation stopped binding. It
// is information to act on, not a decision card, so it carries no buttons;
// warning severity reaches a rule created with the default min_severity.
// One expiry is one incident: the dedup key names the absence it ended.
func FactExpiredEvent(x FactExpiry) Event {
	lines := []string{fmt.Sprintf("Database fact #%d on %s, confirmed by %s, expired: %s.",
		x.FactID, x.Database, x.ConfirmedBy, x.Reason),
		"It no longer binds what pg_sage does."}
	since := int64(0)
	if x.MissingSince != nil {
		since = x.MissingSince.Unix()
		if x.RestoreDays > 0 {
			lines = append(lines, fmt.Sprintf("If the subject comes back with the same "+
				"value within %d days, the confirmation is restored on its own; otherwise "+
				"confirm or declare the fact again.", x.RestoreDays))
		}
	}
	return Event{Type: "fact_expired", Severity: "warning",
		Subject: "Fact no longer binds: " + x.Title, Body: strings.Join(lines, "\n"),
		Data: map[string]any{"database": x.Database, "fact_id": x.FactID, "title": x.Title,
			"confirmed_by": x.ConfirmedBy, "reason": x.Reason},
		DedupKey: fmt.Sprintf("fact_expired:%s:%d:%d", x.Database, x.FactID, since)}
}
