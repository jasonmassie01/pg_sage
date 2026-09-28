// Package srebench is the PGIncidentBench seed (AI-SRE-SPEC §12): fault
// programs that create real incidents on PostgreSQL, a harness that runs
// them through the Sage SRE investigator, and scoring.
package srebench

import (
	"context"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Scenario classes.
const (
	ClassPositive = "positive"
	ClassBenign   = "benign"
	ClassDecoy    = "decoy"
)

// Gold is a scenario's expected diagnosis; an empty root means the right
// answer is "inconclusive".
type Gold struct {
	Root         string
	Contributing []string
}

// Program is a scenario's fault program.
type Program interface {
	Inject(ctx context.Context, e *Env) error
	Manifest(ctx context.Context, e *Env) error
	Between(ctx context.Context, e *Env) error
	// Valid checks, after the investigation, that the scenario's premise
	// held through it; a *Contaminated error has the scenario run again.
	Valid(ctx context.Context, e *Env) error
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

// Outcome is the persisted diagnosis of a run.
type Outcome struct {
	State        sre.State
	Root         string
	Contributing []string
}

// Result is one scenario run. Skipped names a fixture this server
// cannot provide; Err is a broken fault program or harness failure.
// Neither is scored. Attempts counts runs, above 1 when the environment
// broke the scenario's premise.
type Result struct {
	Scenario Scenario
	Outcome  Outcome
	Skipped  string
	Err      error
	Attempts int
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
