package action

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
)

// The read surface for M7's per-family autonomy ledger (AI-SRE-SPEC
// §7.3): which action classes exist, how reversible they are, the highest
// autonomy level M5 grants them, and the verified outcome of every run.

// ActionClassInfo describes one action class.
type ActionClassInfo struct {
	Class         ActionClass `json:"class"`
	Families      []string    `json:"families"`
	RiskTier      string      `json:"risk_tier"`
	Reversibility string      `json:"reversibility"`
	// MaxLevel is the highest autonomy level M5 grants: L2 (one-click
	// approval). Earned autonomy (L3+) is M7's to grant.
	MaxLevel string `json:"max_level"`
}

// ActionClasses lists the M5 action classes.
func ActionClasses() []ActionClassInfo {
	c := executor.CancelBackendRepairContract()
	return []ActionClassInfo{{Class: ActionCancelBackend,
		Families: []string{"lock_blocking", "connection_pressure"},
		RiskTier: c.BaseRiskTier, Reversibility: c.Reversibility, MaxLevel: "L2"}}
}

// ActionOutcome is one run's verified outcome.
type ActionOutcome struct {
	ProposalID      sre.UUID      `json:"proposal_id"`
	InvestigationID sre.UUID      `json:"investigation_id"`
	Family          string        `json:"family"`
	Node            string        `json:"node"`
	Class           ActionClass   `json:"class"`
	Reversibility   string        `json:"reversibility"`
	State           ProposalState `json:"state"`
	Reason          ActionReason  `json:"reason,omitempty"`
	Recovery        RecoveryState `json:"recovery"`
	Attribution     string        `json:"attribution,omitempty"`
	DecidedBy       int           `json:"decided_by,omitempty"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// Outcomes lists the runs (executed, refused, failed or uncertain) changed
// since a time, newest first.
func (a *ActionService) Outcomes(ctx context.Context, since time.Time,
	limit int) ([]ActionOutcome, error) {
	scope, err := a.scope(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	ps, err := a.ps.list(ctx, scope, `state IN ('executed', 'refused',
		'failed', 'uncertain') AND updated_at >= $3 ORDER BY updated_at DESC LIMIT $4`,
		since, limit)
	if err != nil {
		return nil, a.observe(err)
	}
	out := make([]ActionOutcome, 0, len(ps))
	for _, p := range ps {
		out = append(out, ActionOutcome{ProposalID: p.ID, InvestigationID: p.InvestigationID,
			Family: p.Family, Node: p.Node, Class: p.Class,
			Reversibility: p.Contract.Reversibility, State: p.State, Reason: p.Reason,
			Recovery: p.Recovery.State, Attribution: p.Recovery.Attribution,
			DecidedBy: p.DecidedBy, UpdatedAt: p.UpdatedAt})
	}
	return out, nil
}
