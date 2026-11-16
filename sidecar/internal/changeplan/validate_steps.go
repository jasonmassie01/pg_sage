package changeplan

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// kindRules checks what a step runs: an action resolves to an executor
// contract, its SQL passes the executor's allowlist and it carries a
// rollback exactly when the contract is reversible. An operator step
// names what the operator runs and has no rollback pg_sage could run. A
// check step changes nothing, so it carries no SQL and no rollback: it
// reads back its setting or measures its targets. Every statement the
// executor's allowlist accepts is a change (DDL, ALTER SYSTEM, a backend
// signal), so a check step's SQL would be a change that skipped the
// contract and citation rules (fail closed).
func (v *validator) kindRules(s Step) {
	n := s.Ordinal
	switch s.Kind {
	case KindAction:
		v.actionRules(s)
	case KindOperator:
		if s.ProposedSQL == "" {
			v.add(CodeSQLMissing, n, "step %d: an operator step says what the operator runs", n)
		}
		v.noRollback(s, "an operator step cannot be reverted by pg_sage")
	case KindCheck:
		if s.ProposedSQL != "" {
			v.add(CodeSQLNotApplicable, n, "step %d: a check step changes nothing, so it "+
				"carries no SQL; make it an action step with an action type", n)
		}
		v.noRollback(s, "a check step changes nothing")
	default:
		v.add(CodeKindInvalid, n, "step %d: kind %q is not action, operator or check", n, s.Kind)
	}
}

func (v *validator) actionRules(s Step) {
	n := s.Ordinal
	if s.ProposedSQL == "" {
		v.add(CodeSQLMissing, n, "step %d: an action step needs its SQL", n)
	} else {
		v.sqlRule(n, CodeSQLRefused, "proposed", s.ProposedSQL)
	}
	if s.ActionType == "" {
		v.add(CodeActionTypeMissing, n, "step %d: an action step needs an action type", n)
		return
	}
	contract, ok := executor.ContractForActionType(s.ActionType)
	if !ok {
		v.add(CodeActionTypeUnknown, n, "step %d: action type %q has no executor contract",
			n, s.ActionType)
		return
	}
	if policy.RollbackClass(contract.RollbackClass) != policy.RollbackReversible {
		v.noRollback(s, fmt.Sprintf("action type %s is %s", s.ActionType,
			contract.RollbackClass))
		return
	}
	if s.RollbackSQL == "" {
		v.add(CodeRollbackMissing, n, "step %d: action type %s is reversible, so the step "+
			"needs its rollback SQL", n, s.ActionType)
		return
	}
	v.sqlRule(n, CodeRollbackRefused, "rollback", s.RollbackSQL)
}

func (v *validator) sqlRule(n int, code, which, sql string) {
	if err := executor.ValidateExecutorSQL(sql); err != nil {
		v.add(code, n, "step %d: the executor refuses the %s SQL: %v", n, which, err)
	}
}

func (v *validator) noRollback(s Step, why string) {
	if s.RollbackSQL != "" {
		v.add(CodeRollbackNotApplicable, s.Ordinal, "step %d carries rollback SQL, but %s",
			s.Ordinal, why)
	}
}

// measureRules checks what a step's verdict measures.
func (v *validator) measureRules(s Step) {
	n := s.Ordinal
	switch s.Metric {
	case MetricBlocksPerCall, MetricTimePerCall, MetricWALPerTuple:
		if len(s.TargetQueryIDs) == 0 {
			v.add(CodeTargetsMissing, n, "step %d: metric %s needs target query ids", n, s.Metric)
		}
	case MetricReadback:
		if !settingPattern.MatchString(s.Setting) {
			v.add(CodeSettingMissing, n, "step %d: a readback step names the setting it reads "+
				"back (got %q)", n, s.Setting)
		}
	default:
		v.add(CodeMetricInvalid, n, "step %d: metric %q is not blocks_per_call, "+
			"time_per_call, wal_per_tuple or readback", n, s.Metric)
	}
	switch s.ExpectedDirection {
	case DirectionDown, DirectionUp, DirectionNone:
	default:
		v.add(CodeDirectionInvalid, n, "step %d: direction %q is not down, up or none", n,
			s.ExpectedDirection)
	}
	switch s.ExpectedSource {
	case SourceHypoPG, SourceClone, SourceCanary, SourceNone:
	default:
		v.add(CodeSourceInvalid, n, "step %d: source %q is not hypopg, clone, canary or none",
			n, s.ExpectedSource)
	}
	v.expectedPctRule(s)
	v.guardrailRules(s)
	if s.MinEvidence.MinCalls < 0 || s.MinEvidence.MinSamples < 0 {
		v.add(CodeMinEvidenceInvalid, n, "step %d: minimum evidence cannot be negative", n)
	}
}

func (v *validator) expectedPctRule(s Step) {
	if s.ExpectedPct == nil {
		return
	}
	pct := *s.ExpectedPct
	switch {
	case math.IsNaN(pct) || pct <= 0 || pct > maxExpectedPct:
		v.add(CodeExpectedPctInvalid, s.Ordinal, "step %d: the expected change is above 0 "+
			"and at most %.0f%%; it is %v", s.Ordinal, maxExpectedPct, pct)
	case s.ExpectedSource == SourceNone || s.ExpectedDirection == DirectionNone:
		v.add(CodeExpectedPctInvalid, s.Ordinal, "step %d: an expected size needs a source "+
			"and a direction", s.Ordinal)
	}
}

func (v *validator) guardrailRules(s Step) {
	if len(s.Guardrails) > maxGuardrails {
		v.add(CodeGuardrailInvalid, s.Ordinal, "step %d: at most %d guardrails", s.Ordinal,
			maxGuardrails)
	}
	for _, g := range s.Guardrails {
		bad := math.IsNaN(g.MaxRegressPct) || g.MaxRegressPct <= 0 ||
			g.MaxRegressPct > maxExpectedPct
		switch g.Metric {
		case MetricBlocksPerCall, MetricTimePerCall, MetricWALPerTuple:
		default:
			bad = true
		}
		if bad {
			v.add(CodeGuardrailInvalid, s.Ordinal, "step %d: guardrail %s/%v needs a measured "+
				"metric and a limit above 0 and at most %.0f%%", s.Ordinal, g.Metric,
				g.MaxRegressPct, maxExpectedPct)
		}
	}
}

// stepEvidence resolves what a step cites, then applies lab-01's
// up-front rule: targets that ran fewer than the minimum calls in the
// last EvidenceWindow make a step that must declare itself unverifiable.
func (v *validator) stepEvidence(ctx context.Context, r Resolver, s Step) error {
	if s.Kind == KindAction || s.Kind == KindOperator {
		if err := v.citations(ctx, r, s); err != nil {
			return err
		}
	}
	measured := s.Metric != MetricReadback && len(s.TargetQueryIDs) > 0
	declared := s.ExpectedSource == SourceNone && s.OnUnverifiable != ""
	if !measured || declared {
		return nil
	}
	targets := append([]int64(nil), s.TargetQueryIDs...)
	sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
	calls, err := r.TargetCalls(ctx, targets, EvidenceWindow)
	if err != nil {
		return fmt.Errorf("resolve evidence for step %d: target calls: %w", s.Ordinal, err)
	}
	need := s.MinEvidence.MinCalls
	if need == 0 {
		need = DefaultMinCalls
	}
	if calls < need {
		v.add(CodeUnverifiableUndeclared, s.Ordinal, "step %d: its targets ran %d times in "+
			"the last 14 days, fewer than the %d a verdict needs; declare it unverifiable "+
			"(expected_source none and an on_unverifiable action)", s.Ordinal, calls, need)
	}
	return nil
}

// citations: an executable step cites a finding that exists or evidence
// ids in the plan's pack; no step exists only because a model said so.
func (v *validator) citations(ctx context.Context, r Resolver, s Step) error {
	n := s.Ordinal
	if s.FindingID == nil && len(s.EvidenceIDs) == 0 {
		v.add(CodeEvidenceUnresolved, n, "step %d cites no finding and no evidence", n)
		return nil
	}
	if s.FindingID != nil {
		ok, err := r.FindingExists(ctx, *s.FindingID)
		if err != nil {
			return fmt.Errorf("resolve evidence for step %d: finding %d: %w", n,
				*s.FindingID, err)
		}
		if !ok {
			v.add(CodeEvidenceUnresolved, n, "step %d: finding %d does not exist", n,
				*s.FindingID)
		}
	}
	for _, id := range s.EvidenceIDs {
		if v.plan.PackHash == "" {
			v.add(CodeEvidenceUnresolved, n, "step %d: evidence %q needs the plan's pack, "+
				"and the plan has none", n, id)
			continue
		}
		ok, err := r.EvidenceExists(ctx, v.plan.PackHash, id)
		if err != nil {
			return fmt.Errorf("resolve evidence for step %d: %q: %w", n, id, err)
		}
		if !ok {
			v.add(CodeEvidenceUnresolved, n, "step %d: evidence %q is not in pack %s", n, id,
				v.plan.PackHash)
		}
	}
	return nil
}
