package changeplan

import (
	"context"
	"fmt"
	"strings"
)

// maxLandingValueRunes bounds an operator step's expected setting value.
const maxLandingValueRunes = 1000

// operatorRules checks an operator step (plan-02d): it says what the
// operator runs (the command text, a managed proposal's CLI or free
// text), how the engine sees it land, and nothing pg_sage would run or
// revert. pg_sage cannot revert it, so no outcome may revert it: a
// regression halts and asks, with the operator's revert command.
func (v *validator) operatorRules(s Step) {
	n := s.Ordinal
	if strings.TrimSpace(s.OperatorCommand) == "" {
		v.add(CodeCommandMissing, n, "step %d: an operator step says what the operator "+
			"runs (operator_command)", n)
	}
	v.textBound(n, "operator_command", s.OperatorCommand, maxSQLRunes)
	v.textBound(n, "operator_revert", s.OperatorRevert, maxSQLRunes)
	if s.ProposedSQL != "" {
		v.add(CodeSQLNotApplicable, n, "step %d: pg_sage runs nothing for an operator step, "+
			"so it carries no SQL; put what the operator runs in operator_command", n)
	}
	v.noRollback(s, "an operator step cannot be reverted by pg_sage")
	v.landingRules(s)
	for _, o := range []Outcome{OutcomeImproved, OutcomeNoChange, OutcomeRegressed,
		OutcomeInconclusive, OutcomeUnverifiable, OutcomeExpired} {
		switch a := s.actionField(o); a {
		case ActionRevertStep, ActionRevertToCheckpoint:
			v.add(CodeOperatorNotRevertible, n, "step %d: on_%s %s: pg_sage cannot revert "+
				"an operator step; use halt_ask (the halt names operator_revert) or keep",
				n, o, a)
		}
	}
}

// landingRules: a landing check on pg_settings names a setting and its
// expected value; without one the step must be linked to a managed
// change, whose applied status is the landing.
func (v *validator) landingRules(s Step) {
	n := s.Ordinal
	if s.Landing == nil {
		if s.ManagedChangeID == nil {
			v.add(CodeLandingMissing, n, "step %d: an operator step needs a landing check: "+
				"a setting and its expected value, or a managed change", n)
		}
		return
	}
	if !settingPattern.MatchString(s.Landing.Setting) {
		v.add(CodeLandingInvalid, n, "step %d: the landing check names setting %q, which is "+
			"not a setting name", n, s.Landing.Setting)
	}
	if strings.TrimSpace(s.Landing.Value) == "" {
		v.add(CodeLandingInvalid, n, "step %d: the landing check needs the value %s must "+
			"read", n, s.Landing.Setting)
	}
	v.textBound(n, "landing value", s.Landing.Value, maxLandingValueRunes)
}

// noOperatorFields: only an operator step says what an operator runs.
func (v *validator) noOperatorFields(s Step) {
	if s.OperatorCommand != "" || s.OperatorRevert != "" || s.Landing != nil ||
		s.ManagedChangeID != nil {
		v.add(CodeOperatorFieldsNotApplicable, s.Ordinal, "step %d is a %s step; only an "+
			"operator step carries an operator command, revert, landing or managed change",
			s.Ordinal, s.Kind)
	}
}

// landingLookups checks that what an operator step's landing reads
// exists: its setting in pg_settings and its managed change proposal.
func (v *validator) landingLookups(ctx context.Context, r Resolver, s Step) error {
	n := s.Ordinal
	if s.Landing != nil && settingPattern.MatchString(s.Landing.Setting) {
		ok, err := r.SettingExists(ctx, s.Landing.Setting)
		if err != nil {
			return fmt.Errorf("resolve the landing of step %d: setting %s: %w", n,
				s.Landing.Setting, err)
		}
		if !ok {
			v.add(CodeLandingInvalid, n, "step %d: the landing check names setting %s, "+
				"which this server does not have", n, s.Landing.Setting)
		}
	}
	if s.ManagedChangeID != nil {
		ok, err := r.ManagedChangeExists(ctx, *s.ManagedChangeID)
		if err != nil {
			return fmt.Errorf("resolve the landing of step %d: managed change %d: %w", n,
				*s.ManagedChangeID, err)
		}
		if !ok {
			v.add(CodeLandingInvalid, n, "step %d: managed change %d does not exist", n,
				*s.ManagedChangeID)
		}
	}
	return nil
}
