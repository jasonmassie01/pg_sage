package tuner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Refusals of CheckHint and RecordHint.
var (
	// ErrHintsUnavailable: pg_hint_plan or its hint table is missing.
	ErrHintsUnavailable = errors.New("pg_hint_plan hint table unavailable")
	// ErrInvalidHint: not a single statement's valid, allowlisted hint.
	ErrInvalidHint = errors.New("invalid query hint")
	// ErrHintExists: the statement already has a proposed or active hint,
	// or was tuned within the cooldown.
	ErrHintExists = errors.New("statement already has a hint or is cooling down")
	// ErrForeignHint: hint_plan.hints holds a hint for the statement that
	// pg_sage did not install (an operator hint present); pg_sage never
	// replaces it.
	ErrForeignHint = errors.New("statement has a hint pg_sage did not install " +
		"(operator hint present)")
)

// agentSymptom is the symptom sage.query_hints records for agent hints.
const agentSymptom = "tuning_agent"

// HintProposal is a per-statement pg_hint_plan hint the tuning agent
// (roadmap 2.2) proposes. Detail is copied into the finding's detail.
type HintProposal struct {
	QueryID   int64
	Query     string
	Hint      string
	Rationale string
	Detail    map[string]any
}

// HintsAvailable reports whether installed hints change application plans
// (ApplicationEffect, tuning-04) and the hint table is ready.
func (t *Tuner) HintsAvailable() bool {
	if t == nil {
		return false
	}
	h := t.hints()
	return h != nil && h.ApplicationEffect && h.HintTableReady
}

// CheckHint turns an agent hint into a query_tuning finding with the
// tuner's own safeguards: pg_hint_plan syntax, the Set() allowlist and the
// work_mem clamp, the host-memory budget (ErrHintMemory above 5% of host
// RAM, approval_required when host memory is unknown), and one hint per
// statement (proposed or installed hints, an operator's hint, and the
// cooldown). With the hinted EXPLAIN gate on (WithHintComparison) the hint
// must change the plan within the cost tolerance (ErrHintRefused
// otherwise); an unverifiable hint keeps its finding without SQL, with the
// reason in detail.hint_evidence. It has no side
// effects: the agent records only the hints it keeps after its per-cycle
// cap, through RecordHint.
func (t *Tuner) CheckHint(ctx context.Context, p HintProposal) (analyzer.Finding, error) {
	hint, err := t.admitHint(p)
	if err != nil {
		return analyzer.Finding{}, err
	}
	t.mu.Lock()
	err = t.coolingErr(ctx, p.QueryID)
	var f analyzer.Finding
	if err == nil {
		f = t.workMemBudget().annotate(agentHintFinding(p, hint), hint, nil)
	}
	t.mu.Unlock()
	if err != nil {
		return analyzer.Finding{}, err
	}
	// The hinted EXPLAIN runs outside t.mu: it may take its timeouts.
	return t.gateFinding(ctx, f, p.QueryID, p.Query, hint, t.spillPlanFor(ctx, p, hint))
}

// RecordHint records a checked hint in sage.query_hints and cools the
// statement down, so the tuner's deterministic pass leaves it alone. It
// repeats CheckHint's refusals, so a statement hinted since is refused.
func (t *Tuner) RecordHint(ctx context.Context, p HintProposal) error {
	hint, err := t.admitHint(p)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.coolingErr(ctx, p.QueryID); err != nil {
		return err
	}
	t.upsertQueryHint(ctx, p.QueryID, hint, agentSymptom)
	t.recordTuned(p.QueryID)
	return nil
}

// admitHint requires pg_hint_plan and a valid hint; it returns the
// canonical hint.
func (t *Tuner) admitHint(p HintProposal) (string, error) {
	if !t.HintsAvailable() {
		return "", ErrHintsUnavailable
	}
	return t.checkHint(p)
}

// coolingErr refuses a statement in its cooldown, with an open proposal or
// an installed hint (ErrHintExists), or with a hint pg_sage did not install
// (ErrForeignHint). The durable state is read on every call; a failed read
// refuses the hint. The caller holds t.mu.
func (t *Tuner) coolingErr(ctx context.Context, queryID int64) error {
	if _, cooling := t.recentlyTuned[queryID]; cooling {
		return fmt.Errorf("%w: queryid %d", ErrHintExists, queryID)
	}
	states, err := t.hintStates(ctx, []int64{queryID})
	if err != nil {
		return fmt.Errorf("tuner: refusing hint for queryid %d, hint state unknown: %w",
			queryID, err)
	}
	switch states[queryID] {
	case hintForeign:
		return fmt.Errorf("%w: queryid %d", ErrForeignHint, queryID)
	case hintProposed, hintInstalled:
		return fmt.Errorf("%w: queryid %d", ErrHintExists, queryID)
	}
	return nil
}

// checkHint validates the proposal and returns its canonical hint.
func (t *Tuner) checkHint(p HintProposal) (string, error) {
	if p.QueryID == 0 {
		return "", fmt.Errorf("%w: no queryid", ErrInvalidHint)
	}
	if !validateHintSyntax(p.Hint) {
		return "", fmt.Errorf("%w: %q is not pg_hint_plan syntax", ErrInvalidHint, p.Hint)
	}
	hint, err := normalizeSetDirectives(strings.TrimSpace(p.Hint), t.cfg.WorkMemMaxMB)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidHint, err)
	}
	budget := t.workMemBudget()
	if err := budget.refuse(hint); err != nil {
		// The 64 MB floor of hint work_mem can lift a request within the cap
		// above it on a small host: clamp that to the cap, refuse the rest.
		if mb, ok := requestedWorkMemMB(p.Hint); !ok || mb > budget.CeilingMB {
			return "", err
		}
		hint, _ = budget.capHint(hint)
	}
	return hint, nil
}

// agentHintFinding is the query_tuning finding of an accepted agent hint.
func agentHintFinding(p HintProposal, hint string) analyzer.Finding {
	detail := make(map[string]any, len(p.Detail)+4)
	maps.Copy(detail, p.Detail)
	detail["queryid"] = p.QueryID
	detail["query"] = p.Query
	detail["symptoms"] = []string{agentSymptom}
	detail["hint_directive"] = hint
	return analyzer.Finding{
		Category:         "query_tuning",
		Severity:         "warning",
		ObjectType:       "query",
		ObjectIdentifier: fmt.Sprintf("queryid:%d", p.QueryID),
		Title:            fmt.Sprintf("Plan hint for queryid %d", p.QueryID),
		Detail:           detail,
		Recommendation:   p.Rationale,
		RecommendedSQL:   BuildInsertSQL(p.QueryID, hint),
		RollbackSQL:      BuildDeleteSQL(p.QueryID, hint),
		ActionRisk:       "safe",
	}
}
