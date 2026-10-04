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

// HintsAvailable reports whether hints can be installed: pg_hint_plan is
// loaded and its hint table is ready.
func (t *Tuner) HintsAvailable() bool {
	return t != nil && t.hintPlan != nil && t.hintPlan.Available &&
		t.hintPlan.HintTableReady
}

// CheckHint turns an agent hint into a query_tuning finding with the
// tuner's own safeguards: pg_hint_plan syntax, the Set() allowlist and the
// work_mem clamp, and one hint per statement (proposed or active hints and
// the cooldown). It has no side effects: the agent records only the hints
// it keeps after its per-cycle cap, through RecordHint.
func (t *Tuner) CheckHint(ctx context.Context, p HintProposal) (analyzer.Finding, error) {
	hint, err := t.admitHint(p)
	if err != nil {
		return analyzer.Finding{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.coolingErr(ctx, p.QueryID); err != nil {
		return analyzer.Finding{}, err
	}
	return agentHintFinding(p, hint), nil
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
	t.upsertQueryHint(ctx, p.QueryID, hint, agentSymptom, "", "")
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

// coolingErr refuses a statement with a proposed or active hint or in its
// cooldown. The caller holds t.mu.
func (t *Tuner) coolingErr(ctx context.Context, queryID int64) error {
	if len(t.recentlyTuned) == 0 {
		t.loadActiveHints(ctx)
	}
	if _, cooling := t.recentlyTuned[queryID]; cooling {
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
		RollbackSQL:      BuildDeleteSQL(p.QueryID),
		ActionRisk:       "safe",
	}
}
