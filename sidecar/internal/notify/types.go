package notify

// Channel represents a notification delivery channel.
type Channel struct {
	ID      int
	Name    string
	Type    string // "slack", "email", or "pagerduty"
	Config  map[string]string
	Enabled bool
}

// Rule maps events to channels with severity filtering.
type Rule struct {
	ID          int
	ChannelID   int
	Event       string // event type
	MinSeverity string // info, warning, critical
	Enabled     bool
}

// Event is the payload dispatched through the notification system.
type Event struct {
	Type     string         // event type
	Severity string         // info, warning, critical
	Subject  string
	Body     string
	Data     map[string]any // structured payload
}

// ValidEventTypes lists all supported event types.
var ValidEventTypes = map[string]bool{
	"action_executed":  true,
	"action_failed":    true,
	"approval_needed":  true,
	"finding_critical": true,
}

// ValidSeverities lists all supported severity levels with numeric rank.
var ValidSeverities = map[string]int{
	"info":     0,
	"warning":  1,
	"critical": 2,
}

// SeverityMeetsMin returns true if sev >= minSev.
func SeverityMeetsMin(sev, minSev string) bool {
	return ValidSeverities[sev] >= ValidSeverities[minSev]
}
