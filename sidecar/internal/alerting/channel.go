package alerting

import (
	"context"
	"encoding/json"
	"time"
)

// Channel is the interface all alert destinations implement.
type Channel interface {
	Name() string
	Send(ctx context.Context, alert Alert) error
}

// Alert is the payload dispatched to channels.
type Alert struct {
	Findings  []AlertFinding `json:"findings"`
	Severity  string         `json:"severity"`
	Timestamp time.Time      `json:"timestamp"`
	// Database names the monitored database (fleet mode, G7-B09).
	Database string `json:"database,omitempty"`
	// Resolved marks a recovery notification for earlier alerts on the
	// same findings (G7-B14).
	Resolved bool `json:"resolved,omitempty"`
	// Reminder marks a repeat of an alert whose finding is still open;
	// OpenFor is the time since its first delivery (alerting-07).
	Reminder bool          `json:"reminder,omitempty"`
	OpenFor  time.Duration `json:"-"`
}

// MarshalJSON adds open_for_seconds to a reminder's webhook payload.
func (a Alert) MarshalJSON() ([]byte, error) {
	type plain Alert
	return json.Marshal(struct {
		plain
		OpenForSeconds int64 `json:"open_for_seconds,omitempty"`
	}{plain(a), int64(a.OpenFor / time.Second)})
}

// AlertFinding is a finding enriched for alerting.
type AlertFinding struct {
	ID               int64     `json:"id"`
	Category         string    `json:"category"`
	Severity         string    `json:"severity"`
	Title            string    `json:"title"`
	ObjectType       string    `json:"object_type"`
	ObjectIdentifier string    `json:"object_identifier"`
	OccurrenceCount  int       `json:"occurrence_count"`
	Recommendation   string    `json:"recommendation"`
	FirstSeen        time.Time `json:"first_seen"`
	LastSeen         time.Time `json:"last_seen"`
}

// ManagerConfig holds alerting configuration passed from the caller.
type ManagerConfig struct {
	CheckIntervalSeconds int
	CooldownMinutes      int
	// RepeatMinutes is alerting.repeat_minutes; a missing severity takes
	// its default (alerting-07).
	RepeatMinutes   map[string]int
	QuietHoursStart string
	QuietHoursEnd   string
	Timezone        string
	// DatabaseName is stamped on every alert so one Manager per fleet
	// database produces distinguishable notifications (G7-B09).
	DatabaseName string
}

// RouteConfig maps a severity level to channel names.
type RouteConfig struct {
	Severity string
	Channels []string
}
