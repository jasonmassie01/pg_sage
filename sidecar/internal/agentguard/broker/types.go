// Package broker is agent governance's brokered read path (AGENTDB-SPEC
// §6.8): agent_query runs one read statement as the principal's broker
// role (sage_agentb_<id10>), in a session of its own that logs in with the
// broker credential, never through SET ROLE on pg_sage's session. Each
// call is decided by the gate's Decide (kind read, with the objects the
// statement touches), screened, parsed to exactly one SELECT, proven
// side-effect free against the catalog, run read-only under the broker's
// own timeouts and row and byte bounds, masked or refused per column
// class, and recorded in sage.guard_query_audit. It also serves the
// principal's attribution view (G1-10) and agent_whoami.
package broker

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
)

// The read path's vocabulary lives in readapi so transports need not
// import the broker; these aliases keep the broker's own names.
type (
	Request       = readapi.Request
	Column        = readapi.Column
	Result        = readapi.Result
	WhoAmI        = readapi.WhoAmI
	PrincipalView = readapi.PrincipalView
	SponsorView   = readapi.SponsorView
	DatabaseView  = readapi.DatabaseView
	GrantView     = readapi.GrantView
)

// Errors (see readapi).
var (
	ErrInvalid         = readapi.ErrInvalid
	ErrNotPermitted    = readapi.ErrNotPermitted
	ErrUnknownDatabase = readapi.ErrUnknownDatabase
	ErrUnavailable     = readapi.ErrUnavailable
)

// Verdicts and statuses (see readapi).
const (
	VerdictExecute = readapi.VerdictExecute
	VerdictBlocked = readapi.VerdictBlocked
	StatusOK       = readapi.StatusOK
	StatusFailed   = readapi.StatusFailed
)

// Reasons the broker adds to the gate's (agent_*) reasons.
const (
	ReasonForbiddenCharacters = "forbidden_characters"
	ReasonUnsupportedShape    = "unsupported_shape"
	ReasonParserUnavailable   = "parser_unavailable"
	ReasonNotProven           = "read_not_proven"
	ReasonNoRole              = "agent_no_role"
	ReasonEncryptionKey       = "encryption_key_required"
	ReasonDatabaseUnbound     = "database_unbound"
)

// MaskedValue replaces a masked value in a result.
const MaskedValue = "[masked]"

// Target is one monitored database as the broker uses it.
type Target struct {
	Name string
	// DatabaseID is sage.sre_database_bindings.database_id; "" refuses
	// (hashes and audit rows use the id, never the name).
	DatabaseID string
	// Pool is pg_sage's own pool on the database: the broker copies its
	// connection target, and reads classes and writes audit rows with it.
	// Agent SQL never runs on it.
	Pool *pgxpool.Pool
	// ClusterKey keys the principal's roles (§6.6).
	ClusterKey string
	// Env and Verified describe the binding for agent_whoami; the gate's
	// verdict decides the environment a query is treated as.
	Env      envbind.Env
	Verified bool
}

// Targets resolves a fleet database name.
type Targets interface {
	Target(ctx context.Context, name string) (Target, error)
}

// Logins opens a principal's broker credential on a cluster (core's
// Store.BrokerCredential).
type Logins interface {
	BrokerLogin(ctx context.Context, principalID, clusterKey string) (role, password string,
		err error)
}

// Decider is the gate's decision (decide.Decider).
type Decider interface {
	Decide(ctx context.Context, req decide.Request) decide.Verdict
}

// Classes reads a relation's column classes on a target.
type Classes interface {
	Lookup(ctx context.Context, t Target, relid uint32) (classify.RelationClasses, error)
}

// RoleRegistry reports a principal's roles on a cluster (core's Store).
type RoleRegistry interface {
	ClusterRolesOf(ctx context.Context, principalID, clusterKey string) (
		*agentguard.ClusterRole, error)
}

// Deps are the broker's sources. Targets, Logins, Decider, Classes and
// Audit are required; the rest serve agent_whoami and are optional.
type Deps struct {
	Targets Targets
	Logins  Logins
	Decider Decider
	Classes Classes
	Audit   AuditSink
	// Unmasked reports an agents.unmask entry for a column; nil = none.
	Unmasked func(principalID, databaseID string, col classify.Column) bool
	// Roles, Databases and TrustLevel serve agent_whoami.
	Roles      RoleRegistry
	Databases  func(ctx context.Context) []string
	TrustLevel func() string
	// Log receives operator-facing messages (full server error text);
	// nil discards them.
	Log func(level, msg string, args ...any)
}

func (d Deps) validate() error {
	if d.Targets == nil || d.Logins == nil || d.Decider == nil || d.Classes == nil ||
		d.Audit == nil {
		return invalidf("targets, logins, decider, classes and audit are required")
	}
	return nil
}
