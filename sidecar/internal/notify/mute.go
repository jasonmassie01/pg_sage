package notify

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Mutes (ux-09b). A mute silences one class of one database's messages,
// or every class (Class ""), until it ends. "Mute this class" (a message's
// footer) and "Mute database" (the Notifications page) write one. A muted
// message is not sent and not queued; it is counted as suppressed.
//
// The Page messages for the emergency stop, a failed rollback and a freeze
// red line ignore mutes and quiet hours: no one may silence a database
// left in a state no one chose. NeverMuted holds that list and a test
// pins it.
//
// A mute is read once per event at submit time; a failed read sends the
// message (fail open toward delivery), logs the failure and counts it.

// MaxMuteDuration is the longest mute: a forgotten mute must end.
const MaxMuteDuration = 90 * 24 * time.Hour

// maxMuteReason bounds a mute's reason.
const maxMuteReason = 500

// OutcomeMuted is a channel outcome for a message a mute held back.
const OutcomeMuted = "muted"

// DataPageReason marks a Page that must reach a person whatever the mutes
// and quiet hours say; its value is one of the never-muted reasons.
const DataPageReason = "page_reason"

// The never-muted reasons.
const (
	PageEmergencyStop  = "emergency_stop"
	PageRollbackFailed = "rollback_failed"
	PageFreezeRedLine  = "freeze_red_line"
)

// NeverMutedReasons lists the never-muted reasons, sorted.
func NeverMutedReasons() []string {
	out := []string{PageEmergencyStop, PageRollbackFailed, PageFreezeRedLine}
	sort.Strings(out)
	return out
}

// Mute is one row of sage.notification_mutes.
type Mute struct {
	Database  string    `json:"database"`
	Class     string    `json:"class"` // "" mutes every class
	Until     time.Time `json:"until"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// ValidateMute checks a new mute at now: a database, a known class (or
// none), an until in the future and at most 90 days away, an actor, and a
// reason of at most 500 characters.
func ValidateMute(m Mute, now time.Time) error {
	switch {
	case strings.TrimSpace(m.Database) == "":
		return fmt.Errorf("a mute needs a database")
	case m.Class != "" && !Class(m.Class).Valid():
		return fmt.Errorf("unknown mute class %q: use page, ask, digest or none (every class)",
			m.Class)
	case m.Until.IsZero():
		return fmt.Errorf("a mute needs an until time; mutes last at most 90 days")
	case !m.Until.After(now):
		return fmt.Errorf("mute until %s is not in the future", m.Until.UTC().Format(time.RFC3339))
	case m.Until.Sub(now) > MaxMuteDuration:
		return fmt.Errorf("mute until %s is more than 90 days away",
			m.Until.UTC().Format(time.RFC3339))
	case strings.TrimSpace(m.Actor) == "":
		return fmt.Errorf("a mute needs an actor")
	case len(m.Reason) > maxMuteReason:
		return fmt.Errorf("a mute reason is at most %d characters", maxMuteReason)
	}
	return nil
}

// NeverMuted reports whether evt is a Page that ignores mutes and quiet
// hours: a marked emergency stop, failed rollback or freeze red line, an
// action_failed whose rollback failed, or a failing emergency freeze.
func NeverMuted(evt Event) bool {
	switch reason, _ := evt.Data[DataPageReason].(string); reason {
	case PageEmergencyStop, PageRollbackFailed, PageFreezeRedLine:
		return true
	}
	if evt.Type == "action_failed" {
		if failed, _ := evt.Data[DataRollbackFailed].(bool); failed {
			return true
		}
	}
	if evt.Type == "custodian_escalation" {
		feature, _ := evt.Data["feature"].(string)
		profile, _ := evt.Data["profile"].(string)
		return feature == "freeze" && profile == "emergency"
	}
	return false
}

// MuteStore reads and writes mutes.
type MuteStore interface {
	// ActiveMute reports whether database's class c is muted at now: a
	// mute of c or of every class whose until is after now.
	ActiveMute(ctx context.Context, database string, c Class, now time.Time) (bool, error)
}

// muted reports whether a mute holds evt back. A message of no database,
// and a never-muted Page, is never muted; a failed read is not a mute.
func (d *Dispatcher) muted(ctx context.Context, evt Event) bool {
	ms, ok := d.store.(MuteStore)
	db := logDatabase(evt)
	if !ok || db == "" || NeverMuted(evt) {
		return false
	}
	on, err := ms.ActiveMute(ctx, db, ClassOf(evt), d.clock())
	if err != nil {
		d.muteFailures.Add(1)
		d.logFn("ERROR", "%s event for database %q: mute not read, sending it: %v",
			evt.Type, db, err)
		return false
	}
	return on
}

// MuteReadFailures counts the mute reads that failed (and so sent).
func (d *Dispatcher) MuteReadFailures() int64 { return d.muteFailures.Load() }

// MuteLink is the app link that opens the mute dialog for database's class
// c ("" for every class): a message footer's "Mute this class".
func MuteLink(database string, c Class) string {
	q := url.Values{"mute": {database}}
	if c != "" {
		q.Set("class", string(c))
	}
	return "#/notifications?" + q.Encode()
}
