package decide

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// Principals loads a principal with its sponsor and taint state
// (agentguard.Store).
type Principals interface {
	Get(ctx context.Context, id string) (agentguard.Principal, error)
}

// Environments evaluates a database's environment from its bound identity
// (envbind); the agent's claim is never an input.
type Environments interface {
	Environment(ctx context.Context, database string) (envbind.Binding, error)
}

// Profile is a configured agent profile (§9 agents.profiles).
type Profile struct {
	Classes    []Capability
	EnvCeiling envbind.Env
	DirectLane bool
}

// Allows reports whether the profile lists c.
func (p Profile) Allows(c Capability) bool {
	for _, have := range p.Classes {
		if have == c {
			return true
		}
	}
	return false
}

// Profiles resolves a principal's profile by name.
type Profiles interface {
	Profile(name string) (Profile, bool)
}

// Freezes reports a kill or freeze flag on a database or the fleet (D1),
// beyond the principal's own status. The kill workstream supplies it.
type Freezes interface {
	Frozen(ctx context.Context, principalID, database string) (bool, string, error)
}

// ObjectChecker resolves a brokered request's objects to OIDs and checks
// classification and grants (D5). A denial is an *agentguard.DeniedError.
type ObjectChecker interface {
	CheckObjects(ctx context.Context, p agentguard.Principal, database string,
		env envbind.Env, objects []Object) error
}

// Recovery reports a database's recovery posture (D7): PITR and the time
// of the last passed restore drill (zero: none).
type Recovery interface {
	Recovery(ctx context.Context, database string) (pitr bool, lastDrill time.Time, err error)
}

// Leases reports whether a brokered grant still has an unexpired lease in
// the registry (D10). The grants workstream supplies it.
type Leases interface {
	LeaseActive(ctx context.Context, principalID, grantID, database string) (bool, error)
}

// Config wires the decider's sources. Principals is required; every other
// source is optional and its step fails closed when a request needs it
// (D3 treats a missing environment source as prod, D5 and D10 deny, D7
// denies prod writes and DDL), except Freezes and ChangeFreeze, which
// are flags that are off when no source sets them.
type Config struct {
	Principals   Principals
	Environments Environments
	Profiles     Profiles
	Freezes      Freezes
	Objects      ObjectChecker
	// ChangeFreeze reports the operator's change freeze on a database (D6).
	ChangeFreeze func(ctx context.Context, database string) (bool, error)
	Recovery     Recovery
	// Pending counts a principal's approvals still pending (D9).
	Pending func(ctx context.Context, principalID string) (int, error)
	Leases  Leases
	// MaxRequestsPerHour and MaxPendingPerPrincipal are
	// agents.approvals.max_requests_per_hour (30) and
	// max_pending_per_principal (10); 0 takes the default.
	MaxRequestsPerHour     int
	MaxPendingPerPrincipal int
	// RestoreDrillDays is agents.require_restore_drill_days (14); 0 takes
	// the default.
	RestoreDrillDays int
	Now              func() time.Time
}

// Spec defaults (§9).
const (
	DefaultMaxRequestsPerHour     = 30
	DefaultMaxPendingPerPrincipal = 10
	DefaultRestoreDrillDays       = 14
)
