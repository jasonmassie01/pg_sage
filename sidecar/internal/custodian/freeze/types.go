package freeze

import (
	"context"
	"time"
)

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
	Database              string
	Schema                string
	Table                 string
	Threat                ThreatKind
	Urgency               Urgency
	Intent                Intent
	SQL                   string
	Deadline              Deadline
	RequiresAuthorization bool
	ExecuteDirectly       bool
}
type Assessment struct {
	XID       Horizon
	MultiXact Horizon
	Urgency   Urgency
	Deadline  Deadline
	Proposal  Proposal
}
type DatabaseRef struct {
	Name                         string
	AllowConnections, IsTemplate bool
}
type DatabaseCatalog interface {
	ListDatabases(context.Context) ([]DatabaseRef, error)
}
type HorizonReader interface {
	ReadHorizons(context.Context, DatabaseRef) ([]HorizonSample, error)
}
type ProposalSink interface {
	Publish(context.Context, []Proposal) error
}
type ScannerOptions struct {
	Thresholds Thresholds
	Now        func() time.Time
}
type ScanResult struct {
	DatabasesDiscovered int
	DatabasesScanned    int
	DatabasesSkipped    int
	ProposalsPublished  int
	ExecutedActions     int
}
