package changeplan

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Approval modes (plan-03a). A person approves a plan at the hash they
// were shown, in one of three modes:
//
//   - all covers every step except the never-automatic ones
//     (NeedsOwnApproval);
//   - up_to N covers ordinals 1..N except those; N equal to the step count
//     is stored as all;
//   - step covers no step by itself.
//
// A step runs only when its plan's live approval covers it, or it has its
// own live step approval at the current plan hash. Check and operator
// steps change nothing, so any current plan approval covers them. An edit
// voids every approval of the plan, step approvals included.

// Mode is how a plan was approved (sage.plan_approval.mode).
type Mode string

// The approval modes.
const (
	ModeAll  Mode = "all"
	ModeUpTo Mode = "up_to"
	ModeStep Mode = "step"
)

// Via is where a person decided (sage.plan_approval.via).
type Via string

// The decision channels.
const (
	ViaUI   Via = "ui"
	ViaAPI  Via = "api"
	ViaChat Via = "chat"
	ViaCLI  Via = "cli"
)

// What covers a step (StepCoverage.By).
const (
	CoveredByPlan = "plan"
	CoveredByStep = "step"
)

// ErrInvalidDecision refuses a decision before anything is stored; the
// *DecisionError it wraps carries the code an API answers with.
var ErrInvalidDecision = errors.New("changeplan: invalid approval decision")

// The decision error codes.
const (
	CodeHashRequired   = "plan_hash_required"
	CodeInvalidMode    = "invalid_mode"
	CodeInvalidUpTo    = "invalid_up_to"
	CodeInvalidVia     = "invalid_via"
	CodeReasonRequired = "reason_required"
	CodeStepNotInPlan  = "step_not_in_plan"
	CodeNotAnAction    = "not_an_action_step"
)

// DecisionError is one refused input, with its code and what was wrong.
type DecisionError struct {
	Code   string
	Detail string
}

func (e *DecisionError) Error() string { return ErrInvalidDecision.Error() + ": " + e.Detail }

func (e *DecisionError) Unwrap() error { return ErrInvalidDecision }

func refuse(code, format string, args ...any) error {
	return &DecisionError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Decision is what an approver chose: the plan hash they were shown, the
// mode, the last covered ordinal for up_to, and where they decided.
type Decision struct {
	PlanHash string
	Mode     Mode
	UpTo     int
	Via      Via
}

// Check refuses a decision that is malformed on its own (no plan needed).
func (d Decision) Check() error {
	if strings.TrimSpace(d.PlanHash) == "" {
		return refuse(CodeHashRequired, "plan_hash is required: approve the hash you were shown")
	}
	switch d.Mode {
	case ModeAll, ModeStep:
		if d.UpTo != 0 {
			return refuse(CodeInvalidUpTo, "up_to is only for mode up_to, not %s", d.Mode)
		}
	case ModeUpTo:
		if d.UpTo < 1 || d.UpTo > MaxSteps {
			return refuse(CodeInvalidUpTo, "up_to %d is outside 1..%d", d.UpTo, MaxSteps)
		}
	default:
		return refuse(CodeInvalidMode, "mode %q is not all, up_to or step", d.Mode)
	}
	return checkVia(d.Via)
}

func checkVia(v Via) error {
	switch v {
	case ViaUI, ViaAPI, ViaChat, ViaCLI:
		return nil
	}
	return refuse(CodeInvalidVia, "via %q is not ui, api, chat or cli", v)
}

// forPlan is d checked against a plan of steps steps: up_to past the last
// step is refused, and up_to the last step is all.
func (d Decision) forPlan(steps int) (Decision, error) {
	if err := d.Check(); err != nil {
		return Decision{}, err
	}
	if d.Mode != ModeUpTo {
		return d, nil
	}
	if d.UpTo > steps {
		return Decision{}, refuse(CodeInvalidUpTo, "up_to %d is past the plan's %d steps",
			d.UpTo, steps)
	}
	if d.UpTo == steps {
		d.Mode, d.UpTo = ModeAll, 0
	}
	return d, nil
}

// StepApproval is a sage.plan_step_approval row: one person's approval of
// one step at the plan hash they were shown.
type StepApproval struct {
	ID             int64      `json:"id"`
	PlanID         int64      `json:"plan_id"`
	StepID         int64      `json:"step_id"`
	Ordinal        int        `json:"ordinal"`
	PlanHash       string     `json:"plan_hash"`
	ApproverUserID int        `json:"approver_user_id"`
	Via            Via        `json:"via"`
	DecidedAt      time.Time  `json:"decided_at"`
	VoidedAt       *time.Time `json:"voided_at,omitempty"`
	VoidReason     string     `json:"void_reason,omitempty"`
}

// StepCoverage says whether a step may run now as far as approvals go: by
// the plan's approval, by its own, or not (Reason says why).
type StepCoverage struct {
	StepID           int64  `json:"step_id"`
	Ordinal          int    `json:"ordinal"`
	Covered          bool   `json:"covered"`
	By               string `json:"by,omitempty"`
	NeedsOwnApproval bool   `json:"needs_own_approval"`
	Reason           string `json:"reason,omitempty"`
}

// Coverage applies the rule to every step of p: live is the plan's live
// approval (nil without one), steps its step approvals (voided ones and
// those at another hash count for nothing), settings the contexts of the
// settings its steps change.
func Coverage(p Plan, live *Approval, steps []StepApproval,
	settings SettingContexts) []StepCoverage {
	own := map[int64]bool{}
	for _, sa := range steps {
		if sa.VoidedAt == nil && sa.PlanHash == p.PlanHash {
			own[sa.StepID] = true
		}
	}
	if live != nil && live.PlanHash != p.PlanHash {
		live = nil
	}
	out := make([]StepCoverage, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, stepCoverage(s, live, own[s.ID], settings))
	}
	return out
}

// stepCoverage is the rule for one step; live is nil or at the plan's hash.
func stepCoverage(s Step, live *Approval, ownApproval bool,
	settings SettingContexts) StepCoverage {
	c := StepCoverage{StepID: s.ID, Ordinal: s.Ordinal}
	needs, why := NeedsOwnApproval(s, settings)
	c.NeedsOwnApproval = needs
	switch {
	case live == nil:
		c.Reason = "the plan has no live approval at its current hash"
	case ownApproval:
		c.Covered, c.By = true, CoveredByStep
	case s.Kind != KindAction:
		c.Covered, c.By = true, CoveredByPlan
	case needs:
		c.Reason = why
	case live.Mode == ModeStep:
		c.Reason = "the plan is approved step by step: approve this step to run it"
	case live.Mode == ModeUpTo && (live.UpTo == nil || s.Ordinal > *live.UpTo):
		c.Reason = fmt.Sprintf("the plan was approved up to step %d", derefInt(live.UpTo))
	default:
		c.Covered, c.By = true, CoveredByPlan
	}
	return c
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
