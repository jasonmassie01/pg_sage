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
	TotalDatabases   int  `json:"total_databases"`
	Healthy          int  `json:"healthy"`
	Degraded         int  `json:"degraded"`
	TotalFindings    int  `json:"total_findings"`
	TotalCritical    int  `json:"total_critical"`
	TotalActions     int  `json:"total_actions"`
	EmergencyStopped bool `json:"emergency_stopped"`
	// EmergencyStoppedCount is the number of databases currently stopped.
	EmergencyStoppedCount int `json:"emergency_stopped_count"`
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
}
