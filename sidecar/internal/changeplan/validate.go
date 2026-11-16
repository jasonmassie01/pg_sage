package changeplan

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// Problem is one reason a plan is refused: a code, the step's ordinal (0
// for the plan itself, or the foreign ordinal an edge names) and a
// sentence for the operator.
type Problem struct {
	Code    string `json:"code"`
	Ordinal int    `json:"ordinal"`
	Detail  string `json:"detail"`
}

// Problem codes.
const (
	CodeStepCount              = "step_count"
	CodeGoalLength             = "goal_length"
	CodeTextTooLong            = "text_too_long"
	CodeOriginInvalid          = "origin_invalid"
	CodePackHashInvalid        = "pack_hash_invalid"
	CodeOrdinalInvalid         = "ordinal_invalid"
	CodeOrdinalDuplicate       = "ordinal_duplicate"
	CodeEdgeSelf               = "edge_self"
	CodeEdgeForeignStep        = "edge_foreign_step"
	CodeEdgeDuplicate          = "edge_duplicate"
	CodeEdgeCycle              = "edge_cycle"
	CodeKindInvalid            = "kind_invalid"
	CodeActionTypeMissing      = "action_type_missing"
	CodeActionTypeUnknown      = "action_type_unknown"
	CodeSQLMissing             = "sql_missing"
	CodeSQLRefused             = "sql_refused"
	CodeSQLNotApplicable       = "sql_not_applicable"
	CodeRollbackMissing        = "rollback_missing"
	CodeRollbackRefused        = "rollback_refused"
	CodeRollbackNotApplicable  = "rollback_not_applicable"
	CodeEvidenceUnresolved     = "evidence_unresolved"
	CodeTargetsMissing         = "targets_missing"
	CodeSettingMissing         = "setting_missing"
	CodeMetricInvalid          = "metric_invalid"
	CodeDirectionInvalid       = "direction_invalid"
	CodeSourceInvalid          = "source_invalid"
	CodeExpectedPctInvalid     = "expected_pct_invalid"
	CodeGuardrailInvalid       = "guardrail_invalid"
	CodeMinEvidenceInvalid     = "min_evidence_invalid"
	CodeUnverifiableUndeclared = "unverifiable_undeclared"
	CodeDeadlineRange          = "deadline_range"
	CodeOutcomeActionInvalid   = "outcome_action_invalid"
	// CodeCheckpointNotBefore: a step reverts to a checkpoint that its
	// edges make run after it (plan-02c).
	CodeCheckpointNotBefore = "checkpoint_not_before"
	// Operator steps (plan-02d): the command text is missing, the landing
	// check is missing or names what does not exist, an outcome would
	// revert what pg_sage cannot, an action or check step carries operator
	// fields.
	CodeCommandMissing              = "command_missing"
	CodeLandingMissing              = "landing_missing"
	CodeLandingInvalid              = "landing_invalid"
	CodeOperatorNotRevertible       = "operator_not_revertible"
	CodeOperatorFieldsNotApplicable = "operator_fields_not_applicable"
)

// Bounds.
const (
	MaxSteps        = 50
	MaxGoalRunes    = 500
	MaxDeadline     = 43200 // 30 days, in minutes
	maxSQLRunes     = 10000
	maxRefRunes     = 200
	maxListItems    = 100
	maxGuardrails   = 10
	maxExpectedPct  = 1000.0
	maxVersionRunes = 100
)

// EvidenceWindow is how far back a step's targets' calls are counted for
// lab-01's up-front unverifiable rule.
const EvidenceWindow = 14 * 24 * time.Hour

// Resolver answers the lookups Validate needs. A lookup that cannot be
// answered returns an error, which Validate returns as an error (never as
// a plan problem).
type Resolver interface {
	// FindingExists reports whether sage.findings holds id.
	FindingExists(ctx context.Context, id int64) (bool, error)
	// EvidenceExists reports whether the stored pack packHash holds the
	// evidence id (false when there is no such pack).
	EvidenceExists(ctx context.Context, packHash, id string) (bool, error)
	// TargetCalls is how often the query ids ran in the last window.
	TargetCalls(ctx context.Context, queryids []int64, window time.Duration) (int64, error)
	// SettingExists reports whether pg_settings has the setting (an
	// operator step's landing check).
	SettingExists(ctx context.Context, name string) (bool, error)
	// ManagedChangeExists reports whether the managed change proposal id
	// exists (an operator step linked to one).
	ManagedChangeExists(ctx context.Context, id int64) (bool, error)
}

var (
	packHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	settingPattern  = regexp.MustCompile(`^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)?$`)
)

// Validate checks p and returns every problem it has, in plan order. The
// error is a resolver failure ("resolve evidence for step N: ..."); the
// problems found before it are returned with it.
func Validate(ctx context.Context, p Plan, r Resolver) ([]Problem, error) {
	if r == nil {
		return nil, errors.New("changeplan: validate needs a resolver for findings and evidence")
	}
	v := &validator{plan: p}
	v.planFields()
	v.ordinals()
	v.edges()
	v.checkpoints()
	for _, s := range p.Steps {
		v.stepFields(s)
	}
	for _, s := range p.Steps {
		if err := v.stepEvidence(ctx, r, s); err != nil {
			return v.problems, err
		}
	}
	return v.problems, nil
}

type validator struct {
	plan     Plan
	problems []Problem
}

func (v *validator) add(code string, ordinal int, format string, args ...any) {
	v.problems = append(v.problems, Problem{Code: code, Ordinal: ordinal,
		Detail: fmt.Sprintf(format, args...)})
}

func (v *validator) planFields() {
	p := v.plan
	if n := len(p.Steps); n < 1 || n > MaxSteps {
		v.add(CodeStepCount, 0, "a plan has 1 to %d steps; this one has %d", MaxSteps, n)
	}
	if n := utf8.RuneCountInString(p.Goal); n < 1 || n > MaxGoalRunes {
		v.add(CodeGoalLength, 0, "the goal is 1 to %d characters; this one has %d",
			MaxGoalRunes, n)
	}
	switch p.Origin {
	case OriginGenerator, OriginModel, OriginPerson, OriginAgent:
	default:
		v.add(CodeOriginInvalid, 0, "origin %q is not generator, model, person or agent",
			p.Origin)
	}
	if p.PackHash != "" && !packHashPattern.MatchString(p.PackHash) {
		v.add(CodePackHashInvalid, 0, "pack hash %q is not 64 lower-case hex digits",
			p.PackHash)
	}
	v.textBound(0, "origin_ref", p.OriginRef, maxRefRunes)
	v.textBound(0, "model", p.Model, maxRefRunes)
	v.textBound(0, "prompt_version", p.PromptVersion, maxVersionRunes)
	v.textBound(0, "created_by", p.CreatedBy, maxRefRunes)
	if len(p.Scope.Objects) > maxListItems || len(p.Scope.QueryIDs) > maxListItems {
		v.add(CodeTextTooLong, 0, "the scope lists at most %d objects and %d query ids",
			maxListItems, maxListItems)
	}
	for _, o := range p.Scope.Objects {
		v.textBound(0, "scope object", o, maxRefRunes)
	}
}

func (v *validator) textBound(ordinal int, field, s string, max int) {
	if n := utf8.RuneCountInString(s); n > max {
		v.add(CodeTextTooLong, ordinal, "%s is at most %d characters; it has %d", field, max, n)
	}
}

func (v *validator) ordinals() {
	seen := map[int]bool{}
	for _, s := range v.plan.Steps {
		if s.Ordinal < 1 || s.Ordinal > MaxSteps {
			v.add(CodeOrdinalInvalid, s.Ordinal, "step ordinal %d is outside 1..%d",
				s.Ordinal, MaxSteps)
		}
		if seen[s.Ordinal] {
			v.add(CodeOrdinalDuplicate, s.Ordinal, "two steps have ordinal %d", s.Ordinal)
		}
		seen[s.Ordinal] = true
	}
}

func (v *validator) stepFields(s Step) {
	n := s.Ordinal
	if s.DeadlineMinutes < 1 || s.DeadlineMinutes > MaxDeadline {
		v.add(CodeDeadlineRange, n, "step %d: the deadline is 1 to %d minutes; it is %d",
			n, MaxDeadline, s.DeadlineMinutes)
	}
	v.textBound(n, "proposed_sql", s.ProposedSQL, maxSQLRunes)
	v.textBound(n, "rollback_sql", s.RollbackSQL, maxSQLRunes)
	v.textBound(n, "action_type", s.ActionType, maxVersionRunes)
	if len(s.EvidenceIDs) > maxListItems || len(s.TargetQueryIDs) > maxListItems {
		v.add(CodeTextTooLong, n, "step %d lists at most %d evidence ids and %d targets",
			n, maxListItems, maxListItems)
	}
	for _, id := range s.EvidenceIDs {
		v.textBound(n, "evidence id", id, maxRefRunes)
	}
	v.kindRules(s)
	v.measureRules(s)
	v.outcomeRules(s)
}

func (v *validator) outcomeRules(s Step) {
	for _, o := range []Outcome{OutcomeImproved, OutcomeNoChange, OutcomeRegressed,
		OutcomeInconclusive, OutcomeUnverifiable, OutcomeExpired} {
		switch a := s.actionField(o); a {
		case "", ActionKeep, ActionRevertStep, ActionRevertToCheckpoint, ActionHaltAsk:
		default:
			v.add(CodeOutcomeActionInvalid, s.Ordinal,
				"step %d: on_%s %q is not keep, revert_step, revert_to_checkpoint or halt_ask",
				s.Ordinal, o, a)
		}
	}
}
