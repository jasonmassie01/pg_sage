// Package selfconfig derives pg_sage's own settings (intervals, timeouts,
// thresholds) per database from evidence (roadmap phase 3,
// self-configuration). A derivable key the operator leaves unset gets a
// value from its rule, inside bounds that never widen authority or spend;
// the value is recorded in shadow first, compared against the active
// value's measured outcomes for a soak period, and promoted only when the
// comparison is not worse. Every step is written to a derivation ledger
// (sage.config_derivation) with the cited evidence, bounds and rule
// version. The operator's value always wins; "pin current" freezes one.
package selfconfig

import (
	"errors"
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/config"
)

// Widens names the direction in which a key spends more (pg_sage's own
// load on the database, model tokens, investigations) or widens authority.
type Widens int

const (
	// WidensWhenHigher: a larger value spends more.
	WidensWhenHigher Widens = 1
	// WidensWhenLower: a smaller value spends more (intervals, thresholds).
	WidensWhenLower Widens = -1
)

// CapKind says how far past the product default a rule may derive.
type CapKind int

const (
	// CapAtDefault: never past the default in the widening direction.
	CapAtDefault CapKind = iota
	// CapFixed: a fixed hard ceiling past the default (at most 10x),
	// with a recorded justification (CapNote).
	CapFixed
)

// Bounds are the range a derived value is clamped to.
type Bounds struct {
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Default float64 `json:"default"`
	Note    string  `json:"note,omitempty"`
}

// Citation is one number a derivation rests on.
type Citation struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
}

// Derivation is a rule's raw answer before rounding and clamping.
type Derivation struct {
	OK     bool
	Value  float64
	Reason string
}

// Proposal is a rule's final candidate for one database.
type Proposal struct {
	OK       bool
	Value    float64
	Reason   string
	Evidence []Citation
	Bounds   Bounds
}

// Outcome of a shadow comparison.
type Outcome string

const (
	OutcomeNotWorse     Outcome = "not_worse"
	OutcomeWorse        Outcome = "worse"
	OutcomeInsufficient Outcome = "insufficient"
)

// Verdict is the shadow comparison of a candidate against the active value.
type Verdict struct {
	Outcome Outcome
	Reason  string
}

// MinSoakSamples is the fewest soak samples a measurable comparison needs.
const MinSoakSamples = 3

// Rule derives one key. Adding a derived key is one Rule value in Rules().
type Rule struct {
	Key, Name, Unit, Summary string
	// EvidenceDoc and OutcomeDoc describe the rule in the generated docs.
	EvidenceDoc, OutcomeDoc string
	Version                 int
	Widens                  Widens
	Cap                     CapKind
	CapNote                 string
	Step                    float64 // values are rounded up to a multiple
	// Cites names the evidence a derivation of this key rests on.
	Cites            []string
	HardMin, HardMax float64
	// BoundsFor narrows the hard range from other keys (optional).
	BoundsFor func(cfg *config.Config) (lo, hi float64, note string)
	Derive    func(ev Evidence, cfg *config.Config) Derivation
	// Observe returns this pass's soak sample (optional).
	Observe func(ev Evidence, cfg *config.Config) (float64, bool)
	// Compare judges a candidate against the active value from the soak
	// samples; nil means no measurable outcome (the soak itself is the test).
	Compare func(active, candidate float64, samples []float64, cfg *config.Config) Verdict
	Get     func(*config.Config) float64
	Set     func(*config.Config, float64)
}

// Bounds are the rule's final range for cfg: the hard range, narrowed by
// other keys, then capped at the product default in the widening direction.
func (r Rule) Bounds(cfg *config.Config) Bounds {
	def := r.Get(config.DefaultConfig())
	b := Bounds{Min: r.HardMin, Max: r.HardMax, Default: def}
	if r.BoundsFor != nil {
		if cfg == nil {
			cfg = config.DefaultConfig()
		}
		lo, hi, note := r.BoundsFor(cfg)
		b.Min, b.Max, b.Note = math.Max(b.Min, lo), math.Min(b.Max, hi), note
	}
	if r.Cap == CapAtDefault {
		if r.Widens == WidensWhenHigher {
			b.Max = math.Min(b.Max, def)
		} else {
			b.Min = math.Max(b.Min, def)
		}
	}
	return b
}

// Evaluate derives, rounds up to the step and clamps a candidate value.
func (r Rule) Evaluate(ev Evidence, cfg *config.Config) Proposal {
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	b := r.Bounds(cfg)
	p := Proposal{Bounds: b, Evidence: ev.Cite(r.Cites...)}
	if b.Min > b.Max {
		p.Reason = fmt.Sprintf("bounds are empty (%g > %g): %s", b.Min, b.Max, b.Note)
		return p
	}
	d := r.Derive(ev, cfg)
	if !d.OK {
		p.Reason = d.Reason
		return p
	}
	if math.IsNaN(d.Value) {
		p.Reason = "the rule produced no number"
		return p
	}
	p.OK, p.Reason = true, d.Reason
	p.Value = Clamp(roundUp(d.Value, r.Step), b)
	return p
}

// Judge is the shadow comparison: the rule's own when it has a measurable
// outcome and enough samples; otherwise stability over the soak.
func (r Rule) Judge(active, candidate float64, samples []float64,
	cfg *config.Config) Verdict {
	if r.Compare == nil {
		return Verdict{Outcome: OutcomeNotWorse, Reason: "no measurable outcome for this " +
			"key; the candidate held for the whole soak"}
	}
	if len(samples) < MinSoakSamples {
		return Verdict{Outcome: OutcomeInsufficient, Reason: fmt.Sprintf(
			"%d of %d soak samples measured", len(samples), MinSoakSamples)}
	}
	if equal(active, candidate) {
		return Verdict{Outcome: OutcomeWorse, Reason: "the candidate equals the active value"}
	}
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	return r.Compare(active, candidate, samples, cfg)
}

// Clamp keeps v inside b; NaN becomes the default (itself clamped).
func Clamp(v float64, b Bounds) float64 {
	if math.IsNaN(v) {
		v = b.Default
	}
	return math.Min(b.Max, math.Max(b.Min, v))
}

// roundUp rounds v up to a multiple of step, tolerating float noise.
func roundUp(v, step float64) float64 {
	if step <= 0 || math.IsInf(v, 0) {
		return v
	}
	return math.Ceil(v/step-1e-6) * step
}

func equal(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// usable reports whether a measure can be derived from: known, finite and
// not negative.
func usable(m Measure) bool {
	return m.Known && !math.IsNaN(m.Value) && !math.IsInf(m.Value, 0) && m.Value >= 0
}

// ValidateRules refuses a registry that could derive a key it must not:
// every key must be classified derivable with a known lifecycle, and every
// rule complete.
func ValidateRules(rules []Rule) error {
	seen := map[string]bool{}
	var errs []error
	for _, r := range rules {
		if seen[r.Key] {
			errs = append(errs, fmt.Errorf("%s: registered twice", r.Key))
		}
		seen[r.Key] = true
		if err := validateRule(r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Key, err))
		}
	}
	return errors.Join(errs...)
}

func validateRule(r Rule) error {
	if class, ok := config.KeyClassOf(r.Key); !ok || class != config.KeyDerivable {
		return fmt.Errorf("not a derivable key (class %q)", class)
	}
	if _, ok := config.LookupFieldLifecycle(r.Key); !ok {
		return errors.New("no lifecycle")
	}
	switch {
	case r.Name == "" || r.Version < 1:
		return errors.New("needs a name and a version >= 1")
	case r.Widens != WidensWhenHigher && r.Widens != WidensWhenLower:
		return errors.New("widening direction not declared")
	case r.HardMin > r.HardMax || r.Step <= 0:
		return errors.New("hard range inverted or no step")
	case r.Derive == nil || r.Get == nil || r.Set == nil:
		return errors.New("needs Derive, Get and Set")
	case r.Cap == CapFixed && r.CapNote == "":
		return errors.New("a fixed cap past the default needs a justification")
	}
	return nil
}
