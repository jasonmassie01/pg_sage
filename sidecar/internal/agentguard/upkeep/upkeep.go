// Package upkeep runs agent governance's scheduled role jobs on the fleet
// leader (spec §6.4, §6.6, G1-01): dropping a retired agent's
// roles once agents.roles.retire_grace_days have passed, rotating broker
// passwords every agents.broker.rotation_days, and checking connected
// agent backends. Every write is fenced by the leader lease.
//
// The role jobs run core's typed contracts (guard_role_retire,
// guard_role_ensure) through the cluster's executor, under the approval
// of the person who decided them: the admin who retired the principal,
// or whoever approved the principal's last role ensure. The audit marks
// them scheduled. Without such an approver, or when that user is no
// longer an operator or admin, the job reports and skips; it never
// borrows another user's approval and never forces a revoke.
package upkeep

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/executor"
)

// Jobs, as the audit names them.
const (
	JobRetireGrace    = "retire_grace"
	JobBrokerRotation = "broker_rotation"
)

// Finding categories this package raises in each database of a cluster.
const (
	// BackendFindingCategory is a connected agent role that breaks G1-01
	// (critical), keyed by role name.
	BackendFindingCategory = "agent_backend_violation"
	// UpkeepFindingCategory is a scheduled role job that cannot run
	// (warning), keyed by "<job>:<principal id>".
	UpkeepFindingCategory = "agent_role_upkeep"
)

// Outcome reasons this package sets (others are the gate's or core's).
const (
	ReasonNoApprover         = "no_approver"
	ReasonApproverInactive   = "approver_inactive"
	ReasonClusterUnreachable = "cluster_unreachable"
	ReasonUnsupported        = "role_management_unsupported"
	ReasonNoKey              = "encryption_key_required"
	ReasonRetired            = "principal_retired"
	ReasonError              = "error"
)

// DefaultBatch is how many due rows one page reads.
const DefaultBatch = 50

var (
	// ErrInvalid is a runner built without what it needs.
	ErrInvalid = errors.New("upkeep: invalid configuration")
	// ErrFenced is a pass stopped because the leader lease moved.
	ErrFenced = errors.New("upkeep: the leader lease moved; write fenced off")
)

// Roles runs core's role contracts (*agentguard.RoleManager).
type Roles interface {
	Ensure(ctx context.Context, req agentguard.RoleRequest) (agentguard.RoleResult, error)
	Retire(ctx context.Context, req agentguard.RoleRequest) (agentguard.RoleResult, error)
}

// Targets lists the fleet's monitored databases at pass time.
type Targets func(ctx context.Context) ([]agentguard.KillTarget, error)

// Config is the jobs' schedule.
type Config struct {
	RetireGrace time.Duration // agents.roles.retire_grace_days; 0 drops at the next pass
	Rotation    time.Duration // agents.broker.rotation_days
	Batch       int           // rows per page; 0 is DefaultBatch
	// Roles is the login and connection limits the drift reconciler expects
	// (zero: agentguard.DefaultRoleConfig).
	Roles agentguard.RoleConfig
}

// ConfigFrom builds a Config from the day settings.
func ConfigFrom(retireGraceDays, rotationDays int) Config {
	day := 24 * time.Hour
	return Config{RetireGrace: time.Duration(retireGraceDays) * day,
		Rotation: time.Duration(rotationDays) * day, Batch: DefaultBatch,
		Roles: agentguard.DefaultRoleConfig()}
}

// Runner runs the jobs against the governance control database.
type Runner struct {
	control *pgxpool.Pool
	store   *agentguard.Store
	roles   Roles
	targets Targets
	cfg     Config
}

// New returns a runner; every argument is required.
func New(control *pgxpool.Pool, roles Roles, targets Targets, cfg Config) (*Runner, error) {
	switch {
	case control == nil || roles == nil || targets == nil:
		return nil, fmt.Errorf("%w: control database, role manager and targets are "+
			"required", ErrInvalid)
	case cfg.RetireGrace < 0 || cfg.Rotation <= 0 || cfg.Batch < 0:
		return nil, fmt.Errorf("%w: retire grace %v, rotation %v, batch %d", ErrInvalid,
			cfg.RetireGrace, cfg.Rotation, cfg.Batch)
	}
	if cfg.Batch == 0 {
		cfg.Batch = DefaultBatch
	}
	if cfg.Roles == (agentguard.RoleConfig{}) {
		cfg.Roles = agentguard.DefaultRoleConfig()
	}
	return &Runner{control: control, store: agentguard.NewStore(control), roles: roles,
		targets: targets, cfg: cfg}, nil
}

// Outcome is one principal's role job on one cluster.
type Outcome struct {
	PrincipalID string `json:"principal_id"`
	ClusterKey  string `json:"cluster_key"`
	ActionID    int64  `json:"action_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Fix         string `json:"fix,omitempty"`
}

// Report is one pass of a role job.
type Report struct {
	Done       []Outcome `json:"done"`
	Skipped    []Outcome `json:"skipped"`    // waiting for a person or a reachable cluster
	Incomplete []Outcome `json:"incomplete"` // revoke_incomplete: another grantor must revoke
	Withheld   []Outcome `json:"withheld"`   // the gate withheld it (trust level, stop)
	Failed     []Outcome `json:"failed"`
}

const (
	bucketDone       = "done"
	bucketSkipped    = "skipped"
	bucketIncomplete = "incomplete"
	bucketWithheld   = "withheld"
	bucketFailed     = "failed"
)

func (r *Report) add(bucket string, o Outcome) {
	switch bucket {
	case bucketDone:
		r.Done = append(r.Done, o)
	case bucketSkipped:
		r.Skipped = append(r.Skipped, o)
	case bucketIncomplete:
		r.Incomplete = append(r.Incomplete, o)
	case bucketWithheld:
		r.Withheld = append(r.Withheld, o)
	default:
		r.Failed = append(r.Failed, o)
	}
}

// Total counts every outcome of the pass.
func (r Report) Total() int {
	return len(r.Done) + len(r.Skipped) + len(r.Incomplete) + len(r.Withheld) +
		len(r.Failed)
}

// classify sorts a role contract's result into a report bucket.
func classify(o Outcome, err error) (string, Outcome) {
	if err == nil {
		return bucketDone, o
	}
	o.Detail = err.Error()
	var withheld *executor.WithheldError
	if errors.As(err, &withheld) {
		o.Reason = withheld.Decision.BlockedReason
		return bucketWithheld, o
	}
	if d, ok := agentguard.IsDenied(err); ok {
		o.Reason = string(d.Reason)
		if d.Reason == agentguard.ReasonRevokeIncomplete {
			o.Fix = d.Fix
			return bucketIncomplete, o
		}
		return bucketSkipped, o
	}
	switch {
	case errors.Is(err, agentguard.ErrRoleManagementUnsupported):
		o.Reason = ReasonUnsupported
	case errors.Is(err, agentguard.ErrEncryptionKeyRequired):
		o.Reason = ReasonNoKey
	case errors.Is(err, agentguard.ErrRetired):
		o.Reason = ReasonRetired
	default:
		o.Reason = ReasonError
		return bucketFailed, o
	}
	return bucketSkipped, o
}
