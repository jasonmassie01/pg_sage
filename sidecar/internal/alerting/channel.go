package alerting

import (
	"context"
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
	QuietHoursStart      string
	QuietHoursEnd        string
	Timezone             string
}

// RouteConfig maps a severity level to channel names.
type RouteConfig struct {
	Severity string
	Channels []string
}
