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
	Type     string         // event type
	Severity string         // info, warning, critical
	Subject  string
	Body     string
	Data     map[string]any // structured payload
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

// SeverityMeetsMin returns true if sev >= minSev.
func SeverityMeetsMin(sev, minSev string) bool {
	return ValidSeverities[sev] >= ValidSeverities[minSev]
}
