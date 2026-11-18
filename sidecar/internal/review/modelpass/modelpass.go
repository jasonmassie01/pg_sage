// Package modelpass is Sage Review's model pass (review-03a): a model
// ranks the generators' candidates, attacks them, links them, proposes
// hypotheses and writes the report, over N independent agentloop runs
// combined in code (majority or refute). The model reads only the pack's
// model tier and each candidate's numbers, never its SQL; its answer has
// no SQL field, every claim must cite evidence of the run and every number
// in it must appear in that evidence, or the claim is dropped. Without a
// usable model, or when the runs fail, the report is the deterministic
// ranking with templated text, and the pass says why.
package modelpass

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/report"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// PromptVersion names the prompt and answer schema a stored run used.
const PromptVersion = "review-03a/1"

// ErrInvalidSettings is a Settings Review refuses.
var ErrInvalidSettings = errors.New("invalid review model pass settings")

// Settings are review.model_pass, review.model_runs and review.consensus.
type Settings struct {
	Mode      string
	Runs      int
	Consensus string
}

// SettingsFrom reads the review config.
func SettingsFrom(c config.ReviewConfig) Settings {
	return Settings{Mode: c.ModelPass, Runs: c.ModelRuns, Consensus: c.Consensus}
}

func (s Settings) validate() error {
	switch s.Mode {
	case config.ReviewModelPassAuto, config.ReviewModelPassOn, config.ReviewModelPassOff:
	default:
		return fmt.Errorf("%w: mode %q is not auto, on or off", ErrInvalidSettings, s.Mode)
	}
	if s.Runs < 1 || s.Runs > 5 || s.Runs%2 == 0 {
		return fmt.Errorf("%w: %d runs, want an odd number 1-5", ErrInvalidSettings, s.Runs)
	}
	if s.Consensus != config.ReviewConsensusMajority &&
		s.Consensus != config.ReviewConsensusRefute {
		return fmt.Errorf("%w: consensus %q is not majority or refute", ErrInvalidSettings,
			s.Consensus)
	}
	return nil
}

// Recorder stores one finished model run (Store, or a test double).
type Recorder interface {
	RecordRun(ctx context.Context, r RunRecord) error
}

// Deps is what a pass runs with. Model is the review client
// (llm.Manager.ForPurpose("review")) or nil; Fallback is the general
// client when llm.review_llm.fallback_to_general holds (nil: none), and
// serves the pass when Model is not usable (disabled, its daily budget
// spent or its breaker open); Store records every run (nil records
// nothing); Mode is the live review.model_pass, read before each run
// (nil: Settings.Mode); Parallel bounds the runs in flight (0: all);
// OnState sees every state the pass enters; Dropped says whether G-M2
// dropped a model. Budget's zero value is BudgetFor(0).
type Deps struct {
	Model        agentloop.Model
	ModelName    string
	Fallback     agentloop.Model
	FallbackName string
	Price        llm.PriceFunc
	Store        Recorder
	Budget       agentloop.Budget
	Protocol     agentloop.Protocol
	Mode         func() string
	Parallel     int
	OnState      func(report.PassStatus)
	Now          func() time.Time
	// Dropped is review-05a's G-M2 for a model id (nil: never dropped).
	Dropped func(ctx context.Context, model string) (bool, string, error)
}

// DefaultBudget bounds one run.
var DefaultBudget = agentloop.Budget{MaxSteps: 8, MaxCalls: 12, MaxCost: 12,
	Wall: 4 * time.Minute, MaxTokens: 150000, StepTokens: 6000, StepTimeout: 2 * time.Minute}

// BudgetFor is DefaultBudget with each call's completion capped at
// llm.review_llm.max_output_tokens when that is set below the default
// cap (0 keeps the default).
func BudgetFor(maxOutputTokens int) agentloop.Budget {
	b := DefaultBudget
	if maxOutputTokens > 0 && maxOutputTokens < b.StepTokens {
		b.StepTokens = maxOutputTokens
	}
	return b
}

// Input is one review: its run id, the pack's model tier, the generators'
// candidates and the grade.
type Input struct {
	RunID      int64
	Database   string
	Pack       *workpack.Pack
	Candidates []candidate.Candidate
	Grade      *rubric.Grade
}

// Review builds the review's report. The error is a refused Settings or a
// run that could not be recorded (its output is then not used); every
// model failure is a failed or skipped pass in the report instead.
func Review(ctx context.Context, s Settings, d Deps, in Input) (report.Report, error) {
	if err := s.validate(); err != nil {
		return report.Report{}, err
	}
	tr := newTracker()
	tr.onState = d.OnState
	tr.notify(report.PassPending)
	base, err := deterministic(in, s.Runs)
	if err != nil {
		return base, err
	}
	d, status, reason := gate(s, d, in)
	if status == "" {
		status, reason = droppedByGate(ctx, s, d)
	}
	if status != "" {
		return finishFallback(tr, base, status, reason), nil
	}
	if err := tr.advance(report.PassRunning); err != nil {
		return base, err
	}
	ctx = llm.WithPurpose(ctx, llm.PurposeReview)
	p, err := newPass(s, d, in)
	if err != nil {
		return finishFallback(tr, base, report.PassFailed, err.Error()), nil
	}
	out := p.run(ctx)
	if err := p.record(ctx, out.runs); err != nil {
		rep := finishFallback(tr, base, report.PassFailed, err.Error())
		return rep, err
	}
	if out.status != report.PassDone {
		rep := finishFallback(tr, base, out.status, out.reason)
		rep.RunsDone = out.done
		return rep, nil
	}
	rep, err := p.compose(base, out)
	if err != nil {
		return finishFallback(tr, base, report.PassFailed, err.Error()), nil
	}
	_ = tr.advance(report.PassDone)
	return rep, nil
}

// gate says why the pass does not start, or "" when it does, with the
// model it runs on (Fallback when Model is not usable).
func gate(s Settings, d Deps, in Input) (Deps, report.PassStatus, string) {
	switch {
	case s.Mode == config.ReviewModelPassOff:
		return d, report.PassSkipped, "review.model_pass is off"
	case len(in.Candidates) == 0:
		return d, report.PassSkipped, "no candidates to review"
	}
	why, transient := unusable(d.Model)
	if why != "" {
		if fb, _ := unusable(d.Fallback); fb == "" {
			d.Model, d.ModelName = d.Fallback, d.FallbackName
			why = ""
		}
	}
	switch {
	case why != "" && s.Mode == config.ReviewModelPassOn && !transient:
		return d, report.PassFailed, "review.model_pass is on but " + why
	case why != "":
		return d, report.PassSkipped, why
	case in.Pack == nil || in.Pack.Manifest.Tier != workpack.TierModel:
		return d, report.PassFailed, "the evidence pack is not a model tier: the model " +
			"reads only the model tier"
	}
	return d, "", ""
}

// droppedByGate skips the pass when review.model_pass is auto and G-M2
// dropped the model it would run on. An operator's on always runs it, and
// a gate that cannot be read changes nothing (G-M2 only narrows).
func droppedByGate(ctx context.Context, s Settings, d Deps) (report.PassStatus, string) {
	if s.Mode != config.ReviewModelPassAuto || d.Dropped == nil {
		return "", ""
	}
	off, why, err := d.Dropped(ctx, d.ModelName)
	if err != nil || !off {
		return "", ""
	}
	return report.PassSkipped, why
}

// What a model (*llm.Client) can say about itself.
type (
	enabler  interface{ IsEnabled() bool }
	budgeted interface{ IsBudgetExhausted() bool }
	breaker  interface{ IsCircuitOpen() bool }
)

// unusable is why m cannot serve the pass now ("" when it can), and
// whether that passes by itself (a spent budget, an open breaker): such a
// pass is skipped, never failed, whatever review.model_pass says.
func unusable(m agentloop.Model) (string, bool) {
	if m == nil {
		return "no model is configured", false
	}
	if v := reflect.ValueOf(m); v.Kind() == reflect.Pointer && v.IsNil() {
		return "no model is configured", false
	}
	if e, ok := m.(enabler); ok && !e.IsEnabled() {
		return "the model is disabled", false
	}
	if b, ok := m.(budgeted); ok && b.IsBudgetExhausted() {
		return reasonTokenBudget, true
	}
	if b, ok := m.(breaker); ok && b.IsCircuitOpen() {
		return "the model's circuit breaker is open", true
	}
	return "", false
}

// deterministic is the report without a model: every candidate ranked by
// report.Score and each dimension's templated text.
func deterministic(in Input, runs int) (report.Report, error) {
	rep := report.Report{Database: in.Database, Consensus: report.ConsensusDeterministic,
		Runs: runs, Ranked: report.Deterministic(in.Candidates)}
	if in.Grade != nil {
		rep.RubricVersion, rep.Overall = in.Grade.RubricVersion, string(in.Grade.Overall)
	}
	secs, err := report.Sections(in.Grade, rep.Ranked)
	if err != nil {
		return rep, err
	}
	rep.Sections = secs
	return rep, nil
}

func finishFallback(tr *tracker, base report.Report, status report.PassStatus,
	reason string) report.Report {
	_ = tr.advance(status)
	base.ModelPass, base.ModelPassReason = status, reason
	return base
}
