package autonomy

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

type Proposal struct {
	Database      string
	Feature       string
	SQL           string
	Plan          string
	TargetObjects []string
	Deadline      *policy.DeadlineContext
	Evidence      map[string]any
}

type Custodian interface {
	Scan(context.Context) ([]Proposal, error)
}

type ProposalRouter interface {
	Route(context.Context, Proposal) error
}

type VerifiedIndexRouter interface {
	RouteVerifiedIndex(context.Context, Proposal, string, []int64) error
}

type StructuralRehearsalRouter interface {
	RouteStructuralRehearsal(context.Context, Proposal) error
}

type SelfAuditor interface {
	SelfAudit(context.Context) (ledger.AuditResult, error)
}

type SchemaGuard interface {
	Scan(context.Context) (schemaguard.CycleResult, error)
}

type Reporter interface {
	Report(level, message string, fields map[string]any)
}

type DatabaseWorkersConfig struct {
	Database      string
	Interval      time.Duration
	Tick          <-chan time.Time
	Freeze        Custodian
	WAL           Custodian
	Schema        SchemaGuard
	Router        ProposalRouter
	Auditor       SelfAuditor
	Reporter      Reporter
	schemaGate    chan struct{}
	schemaTrigger chan struct{}
}
