// Package readapi is the request and result vocabulary of agent
// governance's brokered read path (agent_query, agent_whoami; AGENTDB-SPEC
// §8.2). It imports nothing from agent governance, so transports (MCP,
// REST) can name it without importing the broker and its gate.
package readapi

import (
	"errors"
	"time"
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
	// RetryAfterSeconds is set on agent_rate (D9): when to retry.
	RetryAfterSeconds int `json:"retry_after,omitempty"`
}

// WhoAmI is agent_whoami (§8.2): the calling principal, and per database
// its environment, lanes, grants and levels.
type WhoAmI struct {
	Principal PrincipalView  `json:"principal"`
	Sponsor   *SponsorView   `json:"sponsor"`
	Databases []DatabaseView `json:"databases"`
	Tainted   bool           `json:"tainted"`
	Frozen    bool           `json:"frozen"`
	// Notice is one line when trust.level keeps agent requests at proposals.
	Notice string `json:"notice,omitempty"`
}

// PrincipalView is the principal as the agent may see it.
type PrincipalView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Profile    string `json:"profile"`
	EnvCeiling string `json:"env_ceiling"`
	Status     string `json:"status"`
}

// SponsorView is the accountable human.
type SponsorView struct {
	UserID int  `json:"user_id"`
	Active bool `json:"active"`
}

// DatabaseView is one database the principal may name.
type DatabaseView struct {
	Name            string         `json:"name"`
	Env             string         `json:"env"`
	BindingVerified bool           `json:"binding_verified"`
	Lanes           []string       `json:"lanes"`
	Grants          []GrantView    `json:"grants"`
	Levels          map[string]int `json:"levels"`
	// Reason is why reads are refused here ("" when allowed).
	Reason string `json:"reason,omitempty"`
}

// GrantView is one privilege of the broker role, read from the catalog.
type GrantView struct {
	Capability string     `json:"capability"`
	Object     string     `json:"object"`
	Columns    []string   `json:"columns,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at"`
}
