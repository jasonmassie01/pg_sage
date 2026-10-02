// Package srebench is PGIncidentBench v1 (AI-SRE-SPEC §12): fault
// programs that create real incidents on PostgreSQL (clean, under
// background noise, and benign decoys), a harness that runs them through
// each arm side by side, per-family scoring with Wilson intervals,
// pre-registered release gates and a JSON plus Markdown report.
package srebench

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Scenario classes.
const (
	// ClassPositive is a clean fault.
	ClassPositive = "positive"
	// ClassNoise is a clean fault under unrelated background load.
	ClassNoise = "noise"
	// ClassDecoy is a benign lookalike of a fault: the right answer is
	// "inconclusive", and any root cause is a false diagnosis.
	ClassDecoy = "decoy"
	// ClassBenign has no fault at all.
	ClassBenign = "benign"
)

// Gold is a scenario's expected diagnosis. An empty root means the right
// answer is "inconclusive" (the evidence is insufficient for any root);
// Lookalike names the mechanism a decoy imitates.
type Gold struct {
	Root         string
	Contributing []string
	Lookalike    string
}

// Sufficient reports whether the scenario's evidence supports a root.
func (g Gold) Sufficient() bool { return g.Root != "" }

// Program is a scenario's fault program.
type Program interface {
	// Inject creates the fault; an *Unsupported error skips the scenario.
	Inject(ctx context.Context, e *Env) error
	// Manifest is the manifestation predicate: the fault (or, for a
	// decoy, its lookalike) is present before the investigation.
	Manifest(ctx context.Context, e *Env) error
	Between(ctx context.Context, e *Env) error
	// Valid checks, after the investigation, that the scenario's premise
	// held through it; a *Contaminated error has the scenario run again.
	Valid(ctx context.Context, e *Env) error
	// Recover removes the fault and verifies it is gone.
	Recover(ctx context.Context, e *Env) error
}

// Scenario is one benchmark case.
type Scenario struct {
	ID      string
	Family  sre.TriggerKind
	Class   string
	Gold    Gold
	Subject string
	Program Program
}

// Outcome is one arm's diagnosis of a run. Ranked lists the hypotheses
// that were not ruled out, in rank order (root first). Timings are set
// only when Measured: FirstEvidence from the investigation's start to its
// first stored evidence, Packet to its conclusion. Forbidden lists the
// forbidden actions the safety grader saw during the investigation.
type Outcome struct {
	State         sre.State
	Root          string
	Contributing  []string
	Ranked        []string
	ProbeCount    int
	Measured      bool
	FirstEvidence time.Duration
	Packet        time.Duration
	Forbidden     []string
	// Model is the model turn's counts (nil for an arm without a model).
	Model *ModelStats
}

// ModelStats counts one investigation's model turn: turns used, accepted
// reviews, fallbacks to the deterministic result and disagreements with
// a conclusive graph.
type ModelStats struct {
	Turns     int `json:"model_turns"`
	Reviewed  int `json:"model_reviewed"`
	Rejected  int `json:"model_rejected"`
	Disagreed int `json:"model_disagreed"`
}

// Result is one arm's run of one scenario in one repeat. Skipped names a
// fixture this server cannot provide; Err is a broken fault program or
// harness failure. Neither is scored. Attempts counts runs, above 1 when
// the environment broke the scenario's premise.
type Result struct {
	Scenario Scenario
	Arm      string
	Repeat   int
	Outcome  Outcome
	Skipped  string
	Err      error
	Attempts int
	evidence []probes.Result
}

// Unsupported reports a fault this server cannot create (a setting or
// permission the fixture needs is off).
type Unsupported struct{ Reason string }

func (u *Unsupported) Error() string { return "unsupported: " + u.Reason }

// Contaminated reports that something outside the fault program broke
// the scenario's premise during the investigation (another session wrote
// cluster-wide WAL in a steady window), so its diagnosis is not a
// measurement of the scenario.
type Contaminated struct{ Reason string }

func (c *Contaminated) Error() string { return "environment contaminated: " + c.Reason }
