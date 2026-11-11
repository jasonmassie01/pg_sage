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
// Source is a subsystem filter: "" applies none, "migration_advisor" is
// reserved and matches nothing, and every other value is a subsystem of the
// finding category registry (internal/findingcat), listed with its
// categories in docs/generated/finding-catalog.md and implemented in
// api.buildSourceClause.
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
