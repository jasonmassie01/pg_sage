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
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// Errors a caller maps to its transport (§8.1). Everything else the agent
// can act on is a Result with a verdict.
var (
	// ErrInvalid is malformed arguments (-32602, 422).
	ErrInvalid = errors.New("broker: invalid request")
	// ErrNotPermitted is a database outside the token's bound (-32003, 403).
	ErrNotPermitted = errors.New("broker: database not permitted for this principal")
	// ErrUnknownDatabase is a database pg_sage does not monitor (-32007, 404).
	ErrUnknownDatabase = errors.New("broker: unknown database")
	// ErrUnavailable is a control database, credential store or audit table
	// that cannot be reached (-32010, 503). No rows leave without an audit.
	ErrUnavailable = errors.New("broker: unavailable")
)

// Verdicts and statuses (§8.1).
const (
	VerdictExecute = "execute"
	VerdictBlocked = "blocked"
	StatusOK       = "ok"
	StatusFailed   = "failed"
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

// Request is one agent_query call.
type Request struct {
	Database string
	SQL      string
	// Params are JSON scalars (string, json.Number, float64, int, bool, nil),
	// bound as text; the server infers their types.
	Params []any
	// MaxRows is 0 for agents.query.max_rows; at most max_rows_ceiling.
	MaxRows int
}

// Column is one output column.
type Column struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Class string `json:"class,omitempty"`
}

// Result is agent_query's answer (§8.2). Values are text, nil for NULL.
type Result struct {
	Verdict      string      `json:"verdict"`
	Status       string      `json:"status,omitempty"`
	ReasonCode   string      `json:"reason_code,omitempty"`
	Detail       string      `json:"detail,omitempty"`
	Fix          string      `json:"fix,omitempty"`
	Columns      []Column    `json:"columns"`
	Rows         [][]*string `json:"rows"`
	Truncated    bool        `json:"truncated"`
	RowCount     int         `json:"row_count"`
	EnvelopeHash string      `json:"envelope_hash,omitempty"`
	Masked       []string    `json:"masked"`
	SQLState     string      `json:"sqlstate,omitempty"`
	Message      string      `json:"message,omitempty"`
	Retryable    bool        `json:"retryable,omitempty"`
}

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
