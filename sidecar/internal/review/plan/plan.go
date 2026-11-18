// Package plan turns one Sage Review into one plan-01 plan (review-03b).
// Generator steps come from the candidates the review kept (by the model
// pass's consensus, or the deterministic ranking without a model). A
// model hypothesis becomes a step only through a typed template: its
// target must resolve in the pack's local tier, the template renders its
// SQL in deterministic code, and the lab-03 ladder's lowest available tier
// must predict at least the minimum improvement. Everything else stays
// advice, with why. No plan step exists only because a model said so.
//
// The package is a producer: plans are stored through changeplan's store
// (validation, caps, hash) and run, if ever, through the plan engine and
// the executor. A model-origin step needs a person's own approval until
// review-05a's G-M1 grants its model authority (changeplan's origin hold).
package plan

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// Who proposed a step or an advice row.
const (
	OriginGenerator = "generator"
	OriginModel     = "model"
	OriginPerson    = "person"
)

// Why a hypothesis or candidate is advice instead of a step
// (sage.review_advice.reason).
const (
	ReasonNoTemplate    = "no_template"
	ReasonNoContract    = "no_contract"
	ReasonUnknownObject = "unknown_object"
	ReasonInvalidTarget = "invalid_target"
	ReasonNoEvidence    = "no_evidence"
	ReasonSQLRefused    = "sql_refused"
	ReasonLadderError   = "ladder_error"
	ReasonNoLadderTier  = "no_ladder_tier"
	ReasonBelowMinimum  = "below_minimum"
	ReasonNotMeasurable = "not_measurable"
	ReasonNextReview    = "next_review"
	ReasonAdviceOnly    = "advice_only"
)

// NoChangesProposed is a review's message when it ends without a plan.
const NoChangesProposed = "no changes proposed"

// CreatedBy is the plans' creator.
const CreatedBy = "sage-review"

// DefaultDeadlineMinutes is how long a step's verdict may take (one day).
const DefaultDeadlineMinutes = 1440

// ErrInvalidInput is an Input Build refuses.
var ErrInvalidInput = errors.New("invalid review plan input")

// Input is one review. Pack is the local tier (the real names); Pseudonyms
// maps the model tier's hashed identifiers back to names (nil without
// review.hash_identifiers). Model is the model id the pass ran on ("" when
// no model ran); ModelAuthority is whether G-M1 is granted for it.
// MinImprovementPct is optimizer.hypopg_min_improvement_pct; Caps the
// policy's plan caps; Settings the pg_settings contexts of the settings a
// step may change. Ladder is the lab-03 ladder (nil: none, so every model
// hypothesis stays advice).
type Input struct {
	RunID             int64
	Database          string
	DatabaseID        *int
	Pack              *workpack.Pack
	Pseudonyms        map[string]string
	Report            report.Report
	Candidates        []candidate.Candidate
	Model             string
	ModelAuthority    bool
	MinImprovementPct float64
	Caps              policy.PlanCaps
	Settings          changeplan.SettingContexts
	Ladder            Ladder
}

// Prediction is what the ladder (or the generator) expects of a step.
type Prediction struct {
	Tier           string   `json:"tier"`
	ImprovementPct *float64 `json:"improvement_pct,omitempty"`
	Metric         string   `json:"metric,omitempty"`
	Direction      string   `json:"direction,omitempty"`
}

// Measurement is the verdict spec handed to the verifier (lab-01).
type Measurement struct {
	Metric          changeplan.Metric    `json:"metric"`
	Direction       changeplan.Direction `json:"direction"`
	TargetQueryIDs  []int64              `json:"target_queryids,omitempty"`
	Setting         string               `json:"setting,omitempty"`
	MinCalls        int64                `json:"min_calls"`
	MinSamples      int                  `json:"min_samples"`
	DeadlineMinutes int                  `json:"deadline_minutes"`
}

// Step is what the review records about one plan step.
type Step struct {
	Ordinal          int         `json:"ordinal"`
	Origin           string      `json:"origin"`
	OriginRef        string      `json:"origin_ref"`
	CandidateID      string      `json:"candidate_id,omitempty"`
	Kind             string      `json:"kind"`
	Target           string      `json:"target"`
	Title            string      `json:"title,omitempty"`
	EvidenceIDs      []string    `json:"evidence_ids"`
	Prediction       Prediction  `json:"prediction"`
	Measurement      Measurement `json:"measurement"`
	ApprovalRequired bool        `json:"approval_required"`
	ApprovalReason   string      `json:"approval_reason,omitempty"`
}

// Advice is a hypothesis or candidate that did not become a step.
type Advice struct {
	Origin      string   `json:"origin"`
	OriginRef   string   `json:"origin_ref,omitempty"`
	Kind        string   `json:"kind"`
	Target      string   `json:"target"`
	Title       string   `json:"title,omitempty"`
	Reason      string   `json:"reason"`
	Detail      string   `json:"detail"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

// Result is a built review plan: Plan is nil (and Message is
// NoChangesProposed) when nothing became a step; Steps are in ordinal
// order; Hypotheses counts the model hypotheses considered.
type Result struct {
	RunID      int64            `json:"run_id"`
	Database   string           `json:"database"`
	Plan       *changeplan.Plan `json:"plan,omitempty"`
	Steps      []Step           `json:"steps"`
	Advice     []Advice         `json:"advice"`
	Hypotheses int              `json:"hypotheses"`
	Message    string           `json:"message,omitempty"`
}

// Build turns in into its plan. The error is an invalid input, an
// unreadable pack or a cancelled context; every refusal of a hypothesis
// or candidate is an advice row instead.
func Build(ctx context.Context, in Input) (Result, error) {
	if err := in.check(); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("build the review plan of run %d: %w", in.RunID, err)
	}
	lp, err := readPack(in.Pack)
	if err != nil {
		return Result{}, fmt.Errorf("build the review plan of run %d: %w", in.RunID, err)
	}
	b := &builder{in: in, lp: lp, res: Result{RunID: in.RunID, Database: in.Database,
		Hypotheses: len(in.Report.Hypotheses), Steps: []Step{}, Advice: []Advice{}}}
	b.generatorSteps()
	if err := b.modelSteps(ctx); err != nil {
		return Result{}, err
	}
	b.assemble()
	return b.res, nil
}

func (in Input) check() error {
	switch {
	case in.RunID <= 0:
		return fmt.Errorf("%w: run id %d", ErrInvalidInput, in.RunID)
	case strings.TrimSpace(in.Database) == "":
		return fmt.Errorf("%w: no database", ErrInvalidInput)
	case in.Pack == nil:
		return fmt.Errorf("%w: no evidence pack (the local tier)", ErrInvalidInput)
	case math.IsNaN(in.MinImprovementPct) || math.IsInf(in.MinImprovementPct, 0) ||
		in.MinImprovementPct < 0:
		return fmt.Errorf("%w: minimum improvement %v%%", ErrInvalidInput, in.MinImprovementPct)
	}
	h, err := in.Pack.Header()
	if err != nil {
		return fmt.Errorf("%w: the pack has no readable header: %v", ErrInvalidInput, err)
	}
	if h.Database != in.Database {
		return fmt.Errorf("%w: the pack is of database %q, the review of %q", ErrInvalidInput,
			h.Database, in.Database)
	}
	return nil
}

// builder holds one Build: proposals in order (generator first, then
// model), then the assembled result.
type builder struct {
	in    Input
	lp    *localPack
	props []proposal
	res   Result
}

// proposal is a step before ordering and caps.
type proposal struct {
	step   changeplan.Step
	info   Step
	tables []string
}

func (b *builder) advise(a Advice) {
	if a.Kind == "" {
		a.Kind = "unknown"
	}
	b.res.Advice = append(b.res.Advice, a)
}

// refusal is why a hypothesis or candidate is advice.
type refusal struct{ reason, detail string }

func (r *refusal) Error() string { return r.reason + ": " + r.detail }

func refuse(reason, format string, args ...any) error {
	return &refusal{reason: reason, detail: fmt.Sprintf(format, args...)}
}
