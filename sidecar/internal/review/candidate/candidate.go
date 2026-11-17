// Package candidate is the contract every Sage Review generator emits
// (review-02a): a proposed step or a piece of advice, its SQL rendered from
// an existing builder, the pack evidence ids it rests on and a ladder
// prediction. Candidates are proposals only: review-03b puts them in a
// plan, and every change still runs through the executor's contract for
// its action type.
package candidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
)

// The action types that are not executor contracts.
const (
	// ActionAdvice is text for a person; pg_sage never runs it.
	ActionAdvice = "advice"
	// ActionOperator is a step an operator runs outside pg_sage.
	ActionOperator = "operator"
)

// How a step may run once planned.
const (
	ApprovalAutomatic = "automatic"
	ApprovalRequired  = "approval_required"
)

// TierNone is a prediction no lab-03 ladder tier backs.
const TierNone = "none"

// ErrInvalid is a candidate Validate or the store refuses.
var ErrInvalid = errors.New("invalid review candidate")

// Prediction is what the step is expected to change. Tier is the lab-03
// ladder tier that measured it (verify.TierName) or TierNone. SizePct is
// nil when the size is unknown. Quantities are the generator's computed
// numbers (bytes reclaimed, write blocks saved a day, ...).
type Prediction struct {
	Metric     string             `json:"metric"`
	Direction  string             `json:"direction"`
	SizePct    *float64           `json:"size_pct,omitempty"`
	Tier       string             `json:"tier"`
	Source     string             `json:"source"`
	Quantities map[string]float64 `json:"quantities,omitempty"`
}

// Candidate is one generator proposal. ID is NewID of its generator,
// kind, target and IDBody, so it is stable across runs. ActionType is
// a registered executor contract, ActionAdvice or ActionOperator.
// Approval is ApprovalAutomatic or ApprovalRequired; Validate forces
// ApprovalRequired on a step spec section 3 says is never automatic and
// says why in ApprovalReason.
type Candidate struct {
	ID             string     `json:"id"`
	Generator      string     `json:"generator"`
	Dimension      string     `json:"dimension"`
	Kind           string     `json:"kind"`
	Target         string     `json:"target"`
	ActionType     string     `json:"action_type"`
	ForwardSQL     string     `json:"forward_sql,omitempty"`
	RollbackSQL    string     `json:"rollback_sql,omitempty"`
	EvidenceIDs    []string   `json:"evidence_ids"`
	Prediction     Prediction `json:"prediction"`
	Reversible     bool       `json:"reversible"`
	Reason         string     `json:"reason"`
	Approval       string     `json:"approval"`
	ApprovalReason string     `json:"approval_reason,omitempty"`
	// Operator is what an ActionOperator candidate asks an operator to run
	// (review-02b); nil on every other candidate.
	Operator *Operator `json:"operator,omitempty"`
}

// Operator is a step pg_sage never runs (plan-02d "you run this"): the
// command an operator runs (a managedparam proposal's CLI, or ALTER
// SYSTEM and a restart), the command that undoes it, and the landing the
// plan engine watches for: Setting reading Value as SHOW prints it.
// Provider and Mechanism name the managed service and how it changes
// settings ("" when self-managed); RebootRequired says the change needs a
// restart.
type Operator struct {
	Command        string `json:"command"`
	Revert         string `json:"revert,omitempty"`
	Setting        string `json:"setting"`
	Value          string `json:"value"`
	Provider       string `json:"provider,omitempty"`
	Mechanism      string `json:"mechanism,omitempty"`
	RebootRequired bool   `json:"reboot_required"`
}

// IDBody is the last part of the candidate's id: its forward SQL, or an
// operator candidate's command (it has no SQL), so two values of one
// setting never share an id.
func (c *Candidate) IDBody() string {
	if c.ActionType == ActionOperator && c.Operator != nil {
		return c.Operator.Command
	}
	return c.ForwardSQL
}

// NewID is the hex SHA-256 of the parts, each ended by a NUL so no two
// splits of the same bytes collide.
func NewID(generator, kind, target, forwardSQL string) string {
	h := sha256.New()
	for _, part := range []string{generator, kind, target, forwardSQL} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Env is what a generator reads besides the pack. WindowDays is
// review.window_days; MinImprovementPct is
// optimizer.hypopg_min_improvement_pct; MaxIndexesPerTable is
// optimizer.max_indexes_per_table.
type Env struct {
	Database           string
	WindowDays         int
	MinImprovementPct  float64
	MaxIndexesPerTable int
}

// Generator turns a pack into candidates. Generate keeps no state between
// calls, so one generator serves many databases at once.
type Generator interface {
	Name() string
	Generate(ctx context.Context, pack *workpack.Pack, env Env) ([]Candidate, Coverage,
		error)
}
