package freeze

import "time"

type Urgency string

const (
	UrgencyGreen Urgency = "green"
	UrgencyAmber Urgency = "amber"
	UrgencyRed   Urgency = "red"
)

type ThreatKind string

const (
	ThreatXID       ThreatKind = "xid"
	ThreatMultiXact ThreatKind = "multixact"
)

type DeadlineKind string

const DeadlineXID DeadlineKind = "xid"

// WraparoundWarnAge is the XID (and multixact) age at which PostgreSQL
// starts warning: 2^31 - 1 - 40,000,000. It is the hard deadline of a
// freeze horizon (urgency stays relative to the freeze maximum).
const WraparoundWarnAge = 2147483647 - 40000000

// WraparoundStopAge is the age at which the server stops assigning XIDs
// (and multixacts): 2^31 - 1 - 3,000,000 (PostgreSQL 14+). It is the hard
// deadline of a horizon already past WraparoundWarnAge.
const WraparoundStopAge = 2147483647 - 3000000

type Intent string

const IntentVacuumFreeze Intent = "vacuum_freeze"

type HorizonSample struct {
	Database            string
	Schema              string
	Table               string
	XIDAge              int64
	XIDMaxAge           int64
	XIDsPerSecond       float64
	MultiXactAge        int64
	MultiXactMaxAge     int64
	MultiXactsPerSecond float64
	// Aggressive are the ages at which a plain VACUUM is aggressive.
	Aggressive AggressiveAges
}
type Thresholds struct{ RedBufferPct, AmberBufferPct float64 }
type Horizon struct {
	Distance int64
	HardAt   time.Time
	Urgency  Urgency
}
type Deadline struct {
	Kind      DeadlineKind
	Threat    ThreatKind
	Urgency   Urgency
	HardAt    time.Time
	Remaining int64
}
type Proposal struct {
	Database string
	Schema   string
	Table    string
	Threat   ThreatKind
	Urgency  Urgency
	Intent   Intent
	Deadline Deadline
}
type Assessment struct {
	XID       Horizon
	MultiXact Horizon
	Urgency   Urgency
	Deadline  Deadline
	Proposal  Proposal
}
