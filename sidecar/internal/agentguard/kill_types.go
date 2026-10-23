package agentguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Kill switch and freeze (spec §6.10, §8.3). A kill freezes every
// agent of its scope at once, across every configured database and
// replica; a freeze does the same for one principal on an operator's
// word. Both are narrowing (§6.2.4): they need no approval and work under
// the emergency stop and at any trust level. Unfreeze widens: it needs an
// operator approval, two people after a kill (§6.11).

// Kill and unfreeze errors, distinguishable with errors.Is.
var (
	// ErrNotFrozen is an unfreeze or release of something not frozen (409).
	ErrNotFrozen = errors.New("agentguard: not frozen")
	// ErrSponsorCannotApprove is the principal's sponsor acting as the
	// second person of a two-person unfreeze (403).
	ErrSponsorCannotApprove = errors.New(
		"agentguard: the principal's sponsor cannot be the second approver")
	// ErrNoFallbackLog is an append to a fallback log that is not
	// configured.
	ErrNoFallbackLog = errors.New("agentguard: no kill fallback log is configured")
)

// KillScope is what a kill covers.
type KillScope string

// Kill scopes (§8.3).
const (
	KillScopeAll       KillScope = "all"
	KillScopePrincipal KillScope = "principal"
	KillScopeDatabase  KillScope = "database"
)

// KillRequest is POST /api/v1/agents/kill. ID is the principal id or the
// database name; Actor is the signed-in admin.
type KillRequest struct {
	Scope  KillScope
	ID     string
	Reason string
	Actor  string
}

// Validate checks the request's shape.
func (r KillRequest) Validate() error {
	switch r.Scope {
	case KillScopeAll:
		if r.ID != "" {
			return invalid("scope all takes no id")
		}
	case KillScopePrincipal:
		if !ValidID(r.ID) {
			return invalid("scope principal needs a principal id (agp_…)")
		}
	case KillScopeDatabase:
		if err := checkText("database", r.ID, 1, 200); err != nil {
			return err
		}
	default:
		return invalid("scope %q must be all, principal or database", r.Scope)
	}
	return checkReasonActor(r.Reason, r.Actor)
}

func checkReasonActor(reason, actor string) error {
	if err := checkText("reason", reason, 1, maxReasonLen); err != nil {
		return err
	}
	return checkText("actor", actor, 1, maxActorLen)
}

// FreezeRequest is POST /api/v1/agents/{id}/freeze.
type FreezeRequest struct {
	PrincipalID string
	Reason      string
	Actor       string
}

// Validate checks the request's shape.
func (r FreezeRequest) Validate() error {
	if !ValidID(r.PrincipalID) {
		return invalid("principal id %q is not valid", r.PrincipalID)
	}
	return checkReasonActor(r.Reason, r.Actor)
}

// Replica is a configured standby of a database (databases[].replicas):
// the kill connects to it with DSN to end agent sessions there.
type Replica struct {
	Name string
	DSN  string
}

// KillTarget is one monitored database the kill reaches. Pool is
// pg_sage's own pool on it; Executor (nil: no gate, the direct path) runs
// the narrowing contracts; ClusterKey groups databases that share roles;
// DatabaseID is the binding's uuid (guard_inflight), "" when unbound.
type KillTarget struct {
	Name       string
	DatabaseID string
	Pool       *pgxpool.Pool
	Executor   Applier
	ClusterKey string
	Replicas   []Replica
}

// MemoryRevoker drops in-memory state of principals at a kill: broker
// pools and their credentials (§6.10 step 2). all is a fleet kill.
type MemoryRevoker interface {
	RevokePrincipals(ctx context.Context, ids []string, all bool)
}

// KillConfig bounds the kill and the unfreeze.
type KillConfig struct {
	// VerifyTimeout is agents.kill_verify_timeout_seconds (10 s).
	VerifyTimeout time.Duration
	// LockTimeout bounds each lock wait: the principals' FOR UPDATE and
	// each ALTER ROLE (2 s), over Attempts tries (3).
	LockTimeout time.Duration
	Attempts    int
	// ReplicaConnectTimeout bounds a connection to a configured replica.
	ReplicaConnectTimeout time.Duration
	// Roles gives the session bounds reported for unconfigured standbys
	// and the attributes an unfreeze falls back to without prior_attrs.
	Roles RoleConfig
	// ApprovalTTL is how long a first unfreeze request waits for the
	// second admin (agents.approvals.ttl_minutes, 15).
	ApprovalTTL time.Duration
	// SingleOperatorMode lets one admin unfreeze after a kill, with a
	// recorded reason (agents.single_operator_mode).
	SingleOperatorMode bool
}

// DefaultKillConfig is the spec §9 defaults.
func DefaultKillConfig() KillConfig {
	return KillConfig{VerifyTimeout: 10 * time.Second, LockTimeout: 2 * time.Second,
		Attempts: 3, ReplicaConnectTimeout: 3 * time.Second, Roles: DefaultRoleConfig(),
		ApprovalTTL: 15 * time.Minute}
}

// Validate checks every bound is positive.
func (c KillConfig) Validate() error {
	switch {
	case c.VerifyTimeout <= 0 || c.LockTimeout <= 0 || c.ReplicaConnectTimeout <= 0:
		return invalid("kill timeouts must be positive")
	case c.Attempts < 1:
		return invalid("kill attempts must be at least 1")
	case c.ApprovalTTL <= 0:
		return invalid("the unfreeze approval TTL must be positive")
	}
	return c.Roles.Validate()
}

// SessionBound is how long a session the kill cannot reach (on an
// unconfigured standby) may live: the agent roles' timeouts (GR-01).
type SessionBound struct {
	StatementTimeoutMS   int64 `json:"statement_timeout_ms"`
	IdleSessionTimeoutMS int64 `json:"idle_session_timeout_ms"`
	// TransactionTimeoutMS is 0 before PostgreSQL 17.
	TransactionTimeoutMS int64 `json:"transaction_timeout_ms"`
}

// SessionBoundFor is the bound the roles carry on a server version.
func SessionBoundFor(rc RoleConfig, serverVersion int) SessionBound {
	b := SessionBound{StatementTimeoutMS: rc.StatementTimeout.Milliseconds(),
		IdleSessionTimeoutMS: rc.IdleSessionTimeout.Milliseconds()}
	if serverVersion >= transactionTimeoutVersion {
		b.TransactionTimeoutMS = rc.TransactionTimeout.Milliseconds()
	}
	return b
}

// RoleAttrs is what a kill changes on a role and an unfreeze restores.
type RoleAttrs struct {
	Login           bool `json:"login"`
	ConnectionLimit int  `json:"connection_limit"`
}

// PriorAttrs maps agent role names to their attributes before a kill
// (guard_cluster_roles.prior_attrs).
type PriorAttrs map[string]RoleAttrs

// ParsePriorAttrs reads stored prior attributes; empty is none.
func ParsePriorAttrs(raw json.RawMessage) (PriorAttrs, error) {
	out := PriorAttrs{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("agentguard: reading prior_attrs: %w", err)
	}
	for role, a := range out {
		if !RolePattern.MatchString(role) {
			return nil, invalid("prior_attrs names %q, not an agent role", role)
		}
		if a.ConnectionLimit < -1 {
			return nil, invalid("prior_attrs of %s: connection limit %d", role,
				a.ConnectionLimit)
		}
	}
	return out, nil
}
