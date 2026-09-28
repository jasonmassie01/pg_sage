package fleet

import "time"

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
