// Package grants runs agent grants (spec §6.3, §6.6, §6.7): the
// typed executor contracts guard_grant and guard_revoke, the grant registry
// sage.guard_grants in each monitored database, the reconciler that revokes
// grants when they expire, and the lease check the broker applies before it
// uses one (D10).
//
// G1 rules: every grant is operator-approved (L2), scoped to the
// principal's broker role, column-listed and time-boxed. In stage and prod
// a grant lists only columns classified clean (classify.Grantable); a
// secret column is never granted. pg_sage grants only what it holds WITH
// GRANT OPTION itself, records the grantor aclexplode shows, and revokes
// GRANTED BY that grantor. A revoke is narrowing: it runs during an
// emergency stop and at every trust level.
package grants

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// Reason codes this package adds to the core's and decide's (§6.2.2,
// §8.1). Lease, ceiling, capability and classification denials use the
// decide reasons; grantor and PUBLIC CREATE denials the core's.
const (
	// ReasonNoRoles is a principal whose broker role is not on the
	// database's cluster yet (guard_role_ensure first).
	ReasonNoRoles agentguard.Reason = "agent_roles_missing"
	// ReasonRevokeIncomplete marks a revoke that left another grantor's
	// privilege on the role (§6.6).
	ReasonRevokeIncomplete agentguard.Reason = "revoke_incomplete"
)

// Errors, distinguishable with errors.Is.
var (
	// ErrFenced is a reconcile write after the leader lease moved.
	ErrFenced = errors.New("grants: the leader lease moved; write fenced off")
	// ErrNotActive is a revoke of a grant that is already revoked.
	ErrNotActive = errors.New("grants: grant is not active")
)

// CapabilityRead is the one capability G1 grants: SELECT on a column list.
// Writes and DDL go through the brokered write path (G2, G3).
const CapabilityRead = "read"

// Grant states (sage.guard_grants.state).
const (
	StateActive           = "active"
	StateRevoked          = "revoked"
	StateRevokeIncomplete = "revoke_incomplete"
)

// Object kinds: a relation grant, or the schema USAGE that comes with the
// first relation grant in a schema and goes with the last.
const (
	KindRelation = "relation"
	KindSchema   = "schema"
)

// LaneBroker is the brokered lane; the direct lane comes in G3.
const LaneBroker = "broker"

// Querier is a pool, connection or transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Target is one monitored database a grant is made in: pg_sage's pool on
// it, its executor (the only path to a change), its SRE binding id and the
// environment envbind evaluated for it (never a claimed one).
type Target struct {
	Name     string
	ID       string // sage.sre_database_bindings.database_id (uuid)
	Pool     *pgxpool.Pool
	Env      envbind.Env
	Executor agentguard.Applier
}

// ObjectRequest is one table or view asked for, as schema.name, with the
// columns wanted; no columns means every column the environment allows.
type ObjectRequest struct {
	Object  string   `json:"object"`
	Columns []string `json:"columns,omitempty"`
}

// GrantRequest is one guard_grant: a capability on objects for a while,
// under an operator's approval.
type GrantRequest struct {
	PrincipalID string
	Target      Target
	Capability  string
	Objects     []ObjectRequest
	Duration    time.Duration
	Approval    agentguard.Approval
	Reason      string
}

// Grant is one registry row.
type Grant struct {
	ID             int64      `json:"id"`
	DatabaseID     string     `json:"database_id"`
	PrincipalID    string     `json:"principal_id"`
	Lane           string     `json:"lane"`
	Capability     string     `json:"capability"`
	ObjectKind     string     `json:"object_kind"`
	ObjectOID      uint32     `json:"object_oid"`
	ObjectName     string     `json:"object"`
	SchemaOID      uint32     `json:"schema_oid"`
	Columns        []string   `json:"columns"`
	Privileges     []string   `json:"privileges"`
	Grantor        string     `json:"grantor"`
	GrantedAt      time.Time  `json:"granted_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	State          string     `json:"state"`
	RevokeDetail   string     `json:"revoke_detail,omitempty"`
	GrantActionID  int64      `json:"grant_action_id"`
	RevokeActionID *int64     `json:"revoke_action_id,omitempty"`
}

// Exclusion is a column the environment kept out of a grant.
type Exclusion struct {
	Object string `json:"object"`
	Column string `json:"column"`
	Reason string `json:"reason"`
}

// GrantResult is what a guard_grant did.
type GrantResult struct {
	ActionID int64       `json:"action_id"`
	Grants   []Grant     `json:"grants"`
	Excluded []Exclusion `json:"excluded,omitempty"`
}

// Revoke causes, recorded on the action.
const (
	CauseExpired  = "expired"
	CauseOperator = "operator"
)

// RevokeRequest is one guard_revoke of a registry row.
type RevokeRequest struct {
	Target  Target
	GrantID int64
	Cause   string
	// ApprovedBy is the operator who revoked it; 0 for the reconciler.
	ApprovedBy int
	// Fence, when set, is the leader lease the reconciler revokes under.
	Fence Fence
}

// RevokeResult is what a guard_revoke did.
type RevokeResult struct {
	ActionID int64                `json:"action_id"`
	Grant    Grant                `json:"grant"`
	Residue  []agentguard.Residue `json:"residue,omitempty"`
	// Schema is the schema USAGE row revoked with the schema's last grant.
	Schema *Grant `json:"schema,omitempty"`
}

// Fence is the leader lease a reconcile pass writes under (v2.3
// leader.Elector.Fence), checked FOR SHARE in Control while each revoke
// commits. An empty Holder writes unfenced (no election configured).
type Fence struct {
	Control *pgxpool.Pool
	Scope   string
	Holder  string
	Epoch   int64
}

// PrincipalSource is the core's principal store (*agentguard.Store).
type PrincipalSource interface {
	Get(ctx context.Context, id string) (agentguard.Principal, error)
	ClusterRoles(ctx context.Context, principalID string) ([]agentguard.ClusterRole, error)
}
