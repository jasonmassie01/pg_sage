// Package backtest measures the verifier (lab-02): it replays placebo
// steps (steps that changed nothing) over a database's stored history and
// counts how often verification calls an effect that did not happen. A
// false positive is an improved or regressed verdict at any look of the
// watch schedule: either would have kept or reverted a change on noise.
// The same placebos with a synthetic 30% drop in blocks per call on their
// targets give the detection rate, so a verifier that never decides
// cannot pass on a false-positive rate of 0.
//
// The replay only reads: snapshot counters and the hourly rollup (through
// verify's CounterSource), the latest statements and tables, and the
// executed actions. Nothing is written to a monitored table but the
// result row (store.go).
package backtest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Defaults and bars of a back-test.
const (
	DefaultDays       = 90
	DefaultPlacebos   = 300
	DefaultMaxRuntime = 10 * time.Minute
	// MinHistoryDays is the stored history a back-test needs: the
	// seasonal match's weeks plus one week to place placebos in.
	MinHistoryDays = 28
	// SeasonalLead is how far into the history the first placebo may
	// fall: the seasonal basis needs baseline.MinWeeks weeks, as a real
	// step on a database this old would have.
	SeasonalLead = 21 * 24 * time.Hour
	// ActionGap keeps a placebo this far from a real executed action on
	// the same tables (the watch's longest window).
	ActionGap = 72 * time.Hour
	// InjectedDrop is the synthetic effect the detection rate measures.
	InjectedDrop = 0.30
	// GlobalShare is one placebo in GlobalShare drawn as a global setting.
	GlobalShare = 4
	maxTargets  = 10
	maxControls = 12
	day         = 24 * time.Hour
)

// Watch criterion of a placebo: the index watch's defaults.
var placeboCriterion = verify.Criterion{MinGainPct: verify.DefaultGainPct,
	RegressPct: verify.DefaultRegressPct, Window: 2 * time.Hour, HardMax: 72 * time.Hour}

// Errors a caller can tell apart.
var (
	ErrNotEnoughHistory = errors.New("not enough history")
	ErrRuntimeExceeded  = errors.New("back-test stopped at verify.backtest_max_runtime")
	ErrAlreadyRunning   = errors.New("back-test already running")
)

// Statement is a statement of the history and the tables it names.
type Statement struct {
	QueryID int64
	Tables  []string
	Calls   int64
}

// Action is a real executed action and the tables it touched; none means
// it may have touched any (a placebo keeps its gap from it everywhere).
type Action struct {
	At     time.Time
	Tables []string
}

// History is what a back-test replays: counters, the extent of the
// stored history, the statements and the executed actions.
type History interface {
	verify.V2Counters
	// Span is the first and last stored counter time; ok is false when
	// none is stored.
	Span(ctx context.Context) (from, to time.Time, ok bool, err error)
	// Statements are the statements as of at, with the tables they name.
	Statements(ctx context.Context, at time.Time) ([]Statement, error)
	// Actions are the actions executed in [from, to).
	Actions(ctx context.Context, from, to time.Time) ([]Action, error)
}

// Config is one back-test run. Days, Placebos and MaxRuntime must be
// positive (Defaults fills zero values); Cycle is the collector cycle.
type Config struct {
	Database   string
	Days       int
	Placebos   int
	Seed       int64
	MaxRuntime time.Duration
	Cycle      time.Duration
	Now        func() time.Time
}

// Defaults fills the zero fields of c with the defaults.
func (c Config) Defaults() Config {
	if c.Days == 0 {
		c.Days = DefaultDays
	}
	if c.Placebos == 0 {
		c.Placebos = DefaultPlacebos
	}
	if c.MaxRuntime == 0 {
		c.MaxRuntime = DefaultMaxRuntime
	}
	if c.Cycle <= 0 {
		c.Cycle = time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Validate refuses a run that cannot give a rate, naming the setting.
func (c Config) Validate() error {
	switch {
	case c.Database == "":
		return errors.New("--database is required: name the database to back-test")
	case c.Days <= 0:
		return fmt.Errorf("--days must be positive, got %d", c.Days)
	case c.Placebos <= 0:
		return fmt.Errorf("--placebos must be positive, got %d", c.Placebos)
	case c.MaxRuntime <= 0:
		return fmt.Errorf("verify.backtest_max_runtime must be positive, got %s",
			c.MaxRuntime)
	}
	return nil
}

// Counts are one verifier's tallies over the placebos.
type Counts struct {
	Placebos       int `json:"placebos"`
	Excluded       int `json:"excluded"`
	FalsePositives int `json:"false_positives"`
	Inconclusive   int `json:"inconclusive"`
	Injected       int `json:"injected"`
	Detected       int `json:"detected"`
}

// Usable is the denominator: the placebos not on an epoch break.
func (c Counts) Usable() int { return c.Placebos - c.Excluded }

// FPRate is the false positives over the usable placebos (0 with none).
func (c Counts) FPRate() float64 { return ratio(c.FalsePositives, c.Usable()) }

// FPWilsonUpper is the Wilson 95% upper bound of FPRate (1 with none).
func (c Counts) FPWilsonUpper() float64 { return WilsonUpper(c.FalsePositives, c.Usable()) }

// InconclusiveShare is the usable placebos the verifier never decided.
func (c Counts) InconclusiveShare() float64 { return ratio(c.Inconclusive, c.Usable()) }

// DetectionRate is the injected placebos the verifier called improved.
func (c Counts) DetectionRate() float64 { return ratio(c.Detected, c.Injected) }

func ratio(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// WilsonUpper is the Wilson score 95% upper bound of k successes in n: 1
// with no trials or all of them successes (exactly, not by rounding).
func WilsonUpper(k, n int) float64 {
	if n <= 0 || k >= n {
		return 1
	}
	const z = 1.959963984540054
	p, fn := float64(k)/float64(n), float64(n)
	den := 1 + z*z/fn
	centre := p + z*z/(2*fn)
	half := z * math.Sqrt(p*(1-p)/fn+z*z/(4*fn*fn))
	return math.Min(1, (centre+half)/den)
}

// Result is one back-test: v2's counts (the row and the gate) and v1's on
// the same windows, for comparison.
type Result struct {
	Database string    `json:"database"`
	RanAt    time.Time `json:"ran_at"`
	From     time.Time `json:"window_from"`
	To       time.Time `json:"window_to"`
	Seed     int64     `json:"seed"`
	Verifier string    `json:"verifier"`
	V2       Counts    `json:"v2"`
	V1       Counts    `json:"v1"`
	Global   int       `json:"global_placebos"`
}
