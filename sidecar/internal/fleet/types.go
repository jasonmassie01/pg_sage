package fleet

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// DatabaseInstance holds the runtime state for a single managed database.
// Direct access to Status fields is NOT safe under concurrent use; callers
// must use UpdateStatus (writers) and SnapshotStatus (readers) so the
// statusMu guards them. See gemini_review.md MEDIUM #73.
type DatabaseInstance struct {
	Name       string
	DatabaseID int
	Config     config.DatabaseConfig
	Pool       *pgxpool.Pool
	Collector  *collector.Collector
	Analyzer   *analyzer.Analyzer
	Executor   *executor.Executor
	Status     *InstanceStatus
	Stopped    bool
	// Cancel stops the per-instance goroutines (collector, analyzer,
	// orchestrator). Set by bootstrap code; called by RemoveInstance.
	// EmergencyStop does not call Cancel because monitoring should
	// continue and Resume must not need to rebuild goroutines.
	Cancel context.CancelFunc

	// statusMu guards reads/writes to the InstanceStatus pointed to by
	// Status. Always access via UpdateStatus / SnapshotStatus.
	statusMu sync.RWMutex
}

// UpdateStatus runs mutator with the underlying *InstanceStatus held
// under a write lock. Safe under concurrency.
func (d *DatabaseInstance) UpdateStatus(mutator func(*InstanceStatus)) {
	d.statusMu.Lock()
	defer d.statusMu.Unlock()
	if d.Status == nil {
		d.Status = &InstanceStatus{}
	}
	mutator(d.Status)
}

// SnapshotStatus returns a shallow copy of the current Status taken
// under the read lock. Callers may read and marshal the returned
// pointer without further synchronisation.
func (d *DatabaseInstance) SnapshotStatus() *InstanceStatus {
	d.statusMu.RLock()
	defer d.statusMu.RUnlock()
	if d.Status == nil {
		return &InstanceStatus{}
	}
	cp := *d.Status
	return &cp
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
	LLMTokensUsed    int       `json:"llm_tokens_used"`
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
	TotalDatabases   int  `json:"total_databases"`
	Healthy          int  `json:"healthy"`
	Degraded         int  `json:"degraded"`
	TotalFindings    int  `json:"total_findings"`
	TotalCritical    int  `json:"total_critical"`
	TotalActions     int  `json:"total_actions"`
	EmergencyStopped bool `json:"emergency_stopped"`
}

// DatabaseStatus pairs a database name with its status.
type DatabaseStatus struct {
	ID         int             `json:"id"`
	DatabaseID int             `json:"database_id"`
	Name       string          `json:"name"`
	Tags       []string        `json:"tags"`
	Status     *InstanceStatus `json:"status"`
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
// Source is a subsystem filter. Accepted values and their mapping to
// sage.findings.category are documented in docs/ui-redesign-v2.md
// §16 and implemented in api.buildFindingsWhere:
//
//	""               — no filter
//	"schema_lint"    — category LIKE 'schema_lint:%'
//	"rules"          — analyzer Tier-1 rule categories
//	"forecaster"     — forecast_* / storage_forecast categories
//	"query_tuning"   — query_tuning / runaway_query / stale_statistics
//	"advisor"        — LLM advisor output (detail->>'subsystem')
//	"optimizer"      — LLM optimizer output (detail->>'subsystem')
//	"migration_advisor" — reserved, not yet emitted
//	"incident"       — incident-class categories
//
// ThematicCategory filters on detail->>'thematic_category' and is
// only meaningful when Source=="schema_lint".
//
// From/To are ISO-8601 timestamps implementing overlapping-window
// semantics on findings: a finding is included when
// created_at <= To AND (resolved_at IS NULL OR resolved_at >= From).
// Both zero → no time filter.
type FindingFilters struct {
	Status           string
	Severity         string
	Category         string
	Source           string
	ThematicCategory string
	Sort             string
	Order            string
	Limit            int
	Offset           int
	From             time.Time
	To               time.Time
}
