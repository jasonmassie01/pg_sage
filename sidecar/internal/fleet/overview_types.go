package fleet

import "time"

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
	// TotalWarning, the incident counts and the collection counts are
	// what the Overview needs before it may say no work is open (cases-11).
	// IncidentsUnknown and FreshnessUnknown count databases whose incidents
	// or collection freshness could not be established.
	TotalWarning      int  `json:"total_warning"`
	IncidentsCritical int  `json:"incidents_critical"`
	IncidentsWarning  int  `json:"incidents_warning"`
	IncidentsUnknown  int  `json:"incidents_unknown"`
	StaleCollection   int  `json:"stale_collection"`
	FreshnessUnknown  int  `json:"freshness_unknown"`
	TotalActions      int  `json:"total_actions"`
	EmergencyStopped  bool `json:"emergency_stopped"`
	// EmergencyStoppedCount is the number of databases currently stopped.
	EmergencyStoppedCount int `json:"emergency_stopped_count"`
	// FleetStopped reports the fleet-wide stop (a stop of all databases):
	// while it holds, every database added is stopped too, and only a
	// resume of all lifts it. FleetStoppedBy/At attribute it.
	FleetStopped   bool       `json:"fleet_stopped"`
	FleetStoppedBy string     `json:"fleet_stopped_by,omitempty"`
	FleetStoppedAt *time.Time `json:"fleet_stopped_at,omitempty"`
}

// DatabaseStatus pairs a database name with its status.
type DatabaseStatus struct {
	ID         int             `json:"id"`
	DatabaseID int             `json:"database_id"`
	Name       string          `json:"name"`
	Tags       []string        `json:"tags"`
	Status     *InstanceStatus `json:"status"`
	// EmergencyStopped reports this database's emergency-stop latch;
	// EmergencyStoppedBy/At attribute it while it is active.
	EmergencyStopped   bool       `json:"emergency_stopped"`
	EmergencyStoppedBy string     `json:"emergency_stopped_by,omitempty"`
	EmergencyStoppedAt *time.Time `json:"emergency_stopped_at,omitempty"`
	// LLMBudget is this database's share of the fleet LLM token budget and
	// its use today; nil (omitted) when no fleet budget is configured or
	// the database has no share (fleet-06).
	LLMBudget *BudgetUsage `json:"llm_budget,omitempty"`
}
