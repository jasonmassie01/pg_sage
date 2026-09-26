package notify

// Channel represents a notification delivery channel.
type Channel struct {
	ID      int               `json:"id"`
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Config  map[string]string `json:"config"`
	Enabled bool              `json:"enabled"`
}

// Rule maps events to channels with severity filtering.
type Rule struct {
	ID          int    `json:"id"`
	ChannelID   int    `json:"channel_id"`
	Event       string `json:"event"`
	MinSeverity string `json:"min_severity"`
	Enabled     bool   `json:"enabled"`
}

// Event is the payload dispatched through the notification system.
type Event struct {
	Type     string // event type
	Severity string // info, warning, critical
	Subject  string
	Body     string
	Data     map[string]any // structured payload
	// DedupKey identifies the incident an event belongs to (e.g. one
	// finding). Empty = derived from type, database and subject.
	DedupKey string
	// Resolve marks a recovery notification for an earlier trigger with
	// the same DedupKey (PagerDuty "resolve", G7-B14).
	Resolve bool
}

// ValidEventTypes lists all supported event types.
var ValidEventTypes = map[string]bool{
	"action_executed":         true,
	"action_failed":           true,
	"approval_needed":         true,
	"finding_critical":        true,
	"query_rewrite_suggested": true,
}

// ValidSeverities lists all supported severity levels with numeric rank.
var ValidSeverities = map[string]int{
	"info":     0,
	"warning":  1,
	"critical": 2,
}

// EventSeverity is the fixed severity each event constructor emits.
// Rules whose min_severity exceeds it can never fire (G7-B06).
var EventSeverity = map[string]string{
	"action_executed":         "info",
	"action_failed":           "warning",
	"approval_needed":         "warning",
	"finding_critical":        "critical",
	"query_rewrite_suggested": "warning",
}

// DefaultMinSeverity is the min_severity a new rule for event gets when
// none is given: the event's own severity, so the rule always fires.
func DefaultMinSeverity(event string) string {
	if sev, ok := EventSeverity[event]; ok {
		return sev
	}
	return "info"
}

// RuleCanFire reports whether a rule (event, minSeverity) can ever
// match an event of that type.
func RuleCanFire(event, minSeverity string) bool {
	sev, ok := EventSeverity[event]
	if !ok {
		return false
	}
	if _, known := ValidSeverities[minSeverity]; !known {
		return false
	}
	return SeverityMeetsMin(sev, minSeverity)
}

// SeverityMeetsMin returns true if sev >= minSev.
func SeverityMeetsMin(sev, minSev string) bool {
	return ValidSeverities[sev] >= ValidSeverities[minSev]
}
