package policy

import (
	"fmt"
	"math/big"
	"slices"
	"time"
)

// PlanCaps is the standing policy's plans block (plan-02b): what one plan,
// and every running plan of a database together, may concentrate, and
// when no plan step may start. A zero field means its default, so a
// document without the block, or with no document at all, gets the
// defaults, never zero caps.
type PlanCaps struct {
	// MaxNewIndexesPerTable bounds the indexes plans build on one table
	// (1-10, default 2).
	MaxNewIndexesPerTable int64
	// MaxNewIndexBytesPct bounds the bytes of the indexes running plans
	// built, as a percentage of the database's size (1-50, default 10).
	MaxNewIndexBytesPct int64
	// MaxRestartBoundSteps is fixed at 1: a plan holds at most one change
	// that takes effect only after a restart.
	MaxRestartBoundSteps int64
	// PeakWindows are window expressions (ParseWindow) inside which no plan
	// step starts. Empty means no peak window.
	PeakWindows []string
}

// The plan caps' defaults and ranges.
const (
	DefaultMaxNewIndexesPerTable = 2
	DefaultMaxNewIndexBytesPct   = 10
	MaxRestartBoundStepsPerPlan  = 1
	maxNewIndexesPerTableLimit   = 10
	maxNewIndexBytesPctLimit     = 50
)

// DefaultPlanCaps is the plans block a document without one gets.
func DefaultPlanCaps() PlanCaps {
	return PlanCaps{MaxNewIndexesPerTable: DefaultMaxNewIndexesPerTable,
		MaxNewIndexBytesPct:  DefaultMaxNewIndexBytesPct,
		MaxRestartBoundSteps: MaxRestartBoundStepsPerPlan}
}

// Resolved is c with every zero field set to its default.
func (c PlanCaps) Resolved() PlanCaps {
	d := DefaultPlanCaps()
	if c.MaxNewIndexesPerTable != 0 {
		d.MaxNewIndexesPerTable = c.MaxNewIndexesPerTable
	}
	if c.MaxNewIndexBytesPct != 0 {
		d.MaxNewIndexBytesPct = c.MaxNewIndexBytesPct
	}
	if c.MaxRestartBoundSteps != 0 {
		d.MaxRestartBoundSteps = c.MaxRestartBoundSteps
	}
	if len(c.PeakWindows) > 0 {
		d.PeakWindows = slices.Clone(c.PeakWindows)
	}
	return d
}

// isDefault reports whether c resolves to the defaults (MarshalDocument
// then writes no plans block).
func (c PlanCaps) isDefault() bool {
	r, d := c.Resolved(), DefaultPlanCaps()
	return r.MaxNewIndexesPerTable == d.MaxNewIndexesPerTable &&
		r.MaxNewIndexBytesPct == d.MaxNewIndexBytesPct &&
		r.MaxRestartBoundSteps == d.MaxRestartBoundSteps && len(r.PeakWindows) == 0
}

// PeakWindowAt is the first peak window open at at, if any. A window that
// no longer parses counts as open (fail closed: ValidateDocument refuses
// it, so only a document built in code can hold one).
func (c PlanCaps) PeakWindowAt(at time.Time) (string, bool) {
	for _, expression := range c.PeakWindows {
		window, err := ParseWindow(expression)
		if err != nil || window.Contains(at) {
			return expression, true
		}
	}
	return "", false
}

// IndexBytesCap is the byte cap on a database of dbBytes (rounded down).
func (c PlanCaps) IndexBytesCap(dbBytes int64) int64 {
	if dbBytes <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(dbBytes), big.NewInt(c.Resolved().MaxNewIndexBytesPct))
	return n.Div(n, big.NewInt(100)).Int64()
}

// IndexBytesWithin reports whether built bytes of new indexes are within
// the byte cap of a database of dbBytes: built*100 <= dbBytes*pct, exact
// at the boundary. An unknown or empty database size allows no bytes.
func (c PlanCaps) IndexBytesWithin(built, dbBytes int64) bool {
	if dbBytes <= 0 {
		return false
	}
	if built <= 0 {
		return true
	}
	lhs := new(big.Int).Mul(big.NewInt(built), big.NewInt(100))
	rhs := new(big.Int).Mul(big.NewInt(dbBytes), big.NewInt(c.Resolved().MaxNewIndexBytesPct))
	return lhs.Cmp(rhs) <= 0
}

// ValidatePlanCaps checks c's ranges; zero fields are the defaults.
func ValidatePlanCaps(c PlanCaps) error {
	if v := c.MaxNewIndexesPerTable; v != 0 && (v < 1 || v > maxNewIndexesPerTableLimit) {
		return fmt.Errorf("plans.max_new_indexes_per_table %d must be 1-%d", v,
			maxNewIndexesPerTableLimit)
	}
	if v := c.MaxNewIndexBytesPct; v != 0 && (v < 1 || v > maxNewIndexBytesPctLimit) {
		return fmt.Errorf("plans.max_new_index_bytes_pct %d must be 1-%d (percent of the "+
			"database size)", v, maxNewIndexBytesPctLimit)
	}
	if v := c.MaxRestartBoundSteps; v != 0 && v != MaxRestartBoundStepsPerPlan {
		return fmt.Errorf("plans.max_restart_bound_steps %d must be %d: a plan holds at "+
			"most one restart-bound step", v, MaxRestartBoundStepsPerPlan)
	}
	for _, expression := range c.PeakWindows {
		if _, err := ParseWindow(expression); err != nil {
			return fmt.Errorf("plans.peak_windows: %w", err)
		}
	}
	return nil
}

// plansWire is the plans block on the wire. Pointers tell an explicit
// value (which must be in range: 0 is refused) from an absent one.
type plansWire struct {
	MaxNewIndexesPerTable *int64   `json:"max_new_indexes_per_table,omitempty"`
	MaxNewIndexBytesPct   *int64   `json:"max_new_index_bytes_pct,omitempty"`
	MaxRestartBoundSteps  *int64   `json:"max_restart_bound_steps,omitempty"`
	PeakWindows           []string `json:"peak_windows,omitempty"`
}

// plansFromWire reads the plans block (nil: the defaults).
func plansFromWire(wire *plansWire) (PlanCaps, error) {
	if wire == nil {
		return PlanCaps{}, nil
	}
	explicit := []struct {
		name  string
		value *int64
	}{{"max_new_indexes_per_table", wire.MaxNewIndexesPerTable},
		{"max_new_index_bytes_pct", wire.MaxNewIndexBytesPct},
		{"max_restart_bound_steps", wire.MaxRestartBoundSteps}}
	for _, field := range explicit {
		if field.value != nil && *field.value == 0 {
			return PlanCaps{}, fmt.Errorf("plans.%s cannot be 0; leave it out for the "+
				"default", field.name)
		}
	}
	caps := PlanCaps{PeakWindows: wire.PeakWindows}
	if wire.MaxNewIndexesPerTable != nil {
		caps.MaxNewIndexesPerTable = *wire.MaxNewIndexesPerTable
	}
	if wire.MaxNewIndexBytesPct != nil {
		caps.MaxNewIndexBytesPct = *wire.MaxNewIndexBytesPct
	}
	if wire.MaxRestartBoundSteps != nil {
		caps.MaxRestartBoundSteps = *wire.MaxRestartBoundSteps
	}
	return caps, ValidatePlanCaps(caps)
}

// plansJSON is the plans block MarshalDocument writes, with every cap
// resolved.
func plansJSON(c PlanCaps) map[string]any {
	r := c.Resolved()
	out := map[string]any{
		"max_new_indexes_per_table": r.MaxNewIndexesPerTable,
		"max_new_index_bytes_pct":   r.MaxNewIndexBytesPct,
		"max_restart_bound_steps":   r.MaxRestartBoundSteps,
	}
	if len(r.PeakWindows) > 0 {
		out["peak_windows"] = r.PeakWindows
	}
	return out
}
