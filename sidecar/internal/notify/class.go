package notify

import "fmt"

// Notification classes (ux-09a). Every event belongs to one of three
// classes, so a new install routes by what a person must do instead of
// building a rule per event type:
//
//   - Page: something is wrong now; sent at once.
//   - Ask: a person is asked to decide (an approval card, a plan, a fact);
//     batched per database.
//   - Digest: everything else; read in the daily digest (ux-02b).
//
// A critical event is always Page, whatever its type: a class may never
// hold back something critical (fail safe). An event type the map does not
// name is Digest unless it is critical.

// Class is an event's delivery class.
type Class string

// The three classes.
const (
	ClassPage   Class = "page"
	ClassAsk    Class = "ask"
	ClassDigest Class = "digest"
)

// Classes lists the classes in their fixed order.
func Classes() []Class { return []Class{ClassPage, ClassAsk, ClassDigest} }

// Valid reports whether c is one of the three classes.
func (c Class) Valid() bool {
	return c == ClassPage || c == ClassAsk || c == ClassDigest
}

// ParseClass reads a class name; anything else is an error naming the
// three classes.
func ParseClass(s string) (Class, error) {
	if c := Class(s); c.Valid() {
		return c, nil
	}
	return "", fmt.Errorf("unknown notification class %q: use page, ask or digest", s)
}

// ClassRuleEvent is the event column of a class rule: "class:<class>",
// which no event type is, so a class rule never matches as an event rule.
func ClassRuleEvent(c Class) string { return "class:" + string(c) }

// eventClasses maps every event type pg_sage sends to its class. A test
// pins it against ValidEventTypes, so a new event type must pick one.
var eventClasses = map[string]Class{
	"finding_critical":          ClassPage,
	"incident_detected":         ClassPage,
	"incident_escalated":        ClassPage,
	"custodian_escalation":      ClassPage,
	"canary_interrupted":        ClassPage,
	EventInvestigationConcluded: ClassPage,
	"approval_needed":           ClassAsk,
	"approval_outcome":          ClassAsk,
	"action_executed":           ClassDigest,
	"action_failed":             ClassDigest,
	"finding_opened":            ClassDigest,
	"finding_resolved":          ClassDigest,
	"incident_resolved":         ClassDigest,
	"query_rewrite_suggested":   ClassDigest,
	"fact_expired":              ClassDigest,
	EventDigestReady:            ClassDigest,
}

// DataRollbackFailed marks an action_failed event whose rollback failed
// too: the database is left in a state no one chose, so it is Page.
const DataRollbackFailed = "rollback_failed"

// ClassOf is evt's delivery class.
func ClassOf(evt Event) Class {
	if evt.Severity == "critical" {
		return ClassPage
	}
	if evt.Type == "action_failed" {
		if failed, _ := evt.Data[DataRollbackFailed].(bool); failed {
			return ClassPage
		}
	}
	if c, ok := eventClasses[evt.Type]; ok {
		return c
	}
	return ClassDigest
}

// EventClass is the class an event type has at its own severity (what a
// rule list or the Rules tab shows).
func EventClass(eventType string) Class {
	return ClassOf(Event{Type: eventType, Severity: EventSeverity[eventType]})
}
