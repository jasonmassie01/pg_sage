package fleet

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// DatabaseInstance holds the runtime state for a single managed database.
type DatabaseInstance struct {
	Name      string
	Config    config.DatabaseConfig
	Pool      *pgxpool.Pool
	Collector *collector.Collector
	Analyzer  *analyzer.Analyzer
	Executor  *executor.Executor
	Status    *InstanceStatus
	Stopped bool
	cancel  context.CancelFunc
}

// InstanceStatus tracks the health of a single database.
type InstanceStatus struct {
	Connected        bool      `json:"connected"`
	PGVersion        string    `json:"pg_version"`
	Platform         string    `json:"platform"`
	DatabaseSize     int64     `json:"database_size_bytes"`
	TrustLevel       string    `json:"trust_level"`
	CollectorLastRun time.Time `json:"collector_last_run"`
	AnalyzerLastRun  time.Time `json:"analyzer_last_run"`
	FindingsOpen     int       `json:"findings_open"`
	FindingsCritical int       `json:"findings_critical"`
	FindingsWarning  int       `json:"findings_warning"`
	FindingsInfo     int       `json:"findings_info"`
	ActionsTotal     int       `json:"actions_total"`
	LLMTokensUsed   int       `json:"llm_tokens_used"`
	AdvisoryLockHeld bool      `json:"advisory_lock_held"`
	HealthScore      int       `json:"health_score"`
	Error            string    `json:"error,omitempty"`
	LastSeen         time.Time `json:"last_seen"`
	DatabaseName     string    `json:"database_name"`
}

// FleetOverview is the response for the fleet status endpoint.
type FleetOverview struct {
	Mode      string           `json:"mode"`
	Summary   FleetSummary     `json:"summary"`
	Databases []DatabaseStatus `json:"databases"`
}

// FleetSummary aggregates fleet-wide metrics.
type FleetSummary struct {
	TotalDatabases int `json:"total_databases"`
	Healthy        int `json:"healthy"`
	Degraded       int `json:"degraded"`
	TotalFindings  int `json:"total_findings"`
	TotalCritical  int `json:"total_critical"`
	TotalActions   int `json:"total_actions"`
}

// DatabaseStatus pairs a database name with its status.
type DatabaseStatus struct {
	Name   string          `json:"name"`
	Tags   []string        `json:"tags"`
	Status *InstanceStatus `json:"status"`
}

// FindingRow is a finding as returned from the database.
type FindingRow struct {
	ID               string         `json:"id"`
	CreatedAt        time.Time      `json:"created_at"`
	LastSeen         time.Time      `json:"last_seen"`
	OccurrenceCount  int            `json:"occurrence_count"`
	Category         string         `json:"category"`
	Severity         string         `json:"severity"`
	ObjectType       string         `json:"object_type"`
	ObjectIdentifier string         `json:"object_identifier"`
	Title            string         `json:"title"`
	Detail           map[string]any `json:"detail"`
	Recommendation   string         `json:"recommendation"`
	RecommendedSQL   string         `json:"recommended_sql"`
	RollbackSQL      string         `json:"rollback_sql"`
	Status           string         `json:"status"`
	ResolvedAt       *time.Time     `json:"resolved_at,omitempty"`
	DatabaseName     string         `json:"database_name"`
}

// ActionRow is an action as returned from the database.
type ActionRow struct {
	ID           string    `json:"id"`
	ExecutedAt   time.Time `json:"executed_at"`
	ActionType   string    `json:"action_type"`
	FindingID    *string   `json:"finding_id,omitempty"`
	SQLExecuted  string    `json:"sql_executed"`
	RollbackSQL  string    `json:"rollback_sql,omitempty"`
	BeforeState  string    `json:"before_state,omitempty"`
	AfterState   string    `json:"after_state,omitempty"`
	Outcome      string    `json:"outcome"`
	DatabaseName string    `json:"database_name"`
}

// FindingFilters are query parameters for finding listings.
type FindingFilters struct {
	Status   string
	Severity string
	Category string
	Sort     string
	Order    string
	Limit    int
	Offset   int
}
