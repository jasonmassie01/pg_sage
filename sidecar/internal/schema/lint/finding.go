package lint

import (
	"time"
)

// Finding represents a single schema anti-pattern detection result.
type Finding struct {
	RuleID      string    `json:"rule_id"`
	Schema      string    `json:"schema"`
	Table       string    `json:"table"`
	Column      string    `json:"column,omitempty"`
	Index       string    `json:"index,omitempty"`
	Severity    string    `json:"severity"`
	Category    string    `json:"category"`
	Description string    `json:"description"`
	Impact      string    `json:"impact,omitempty"`
	TableSize   int64     `json:"table_size,omitempty"`
	QueryCount  int64     `json:"query_count,omitempty"`
	Suggestion  string    `json:"suggestion,omitempty"`
	SQL         string    `json:"sql,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
}
