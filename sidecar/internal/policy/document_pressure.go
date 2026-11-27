package policy

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// PressurePolicy is the standing policy's pressure block (guard-01a): the
// slow and stop limits check_pressure answers a backfill against, and the
// inputs whose being unknown is itself a stop. A zero block means the
// defaults (half of each matching alert threshold, internal/pressure).
type PressurePolicy struct {
	// Limits overrides an input's slow or stop bound; an unset bound keeps
	// its default.
	Limits map[string]PressureLimit
	// Required are the inputs that stop the answer when unknown. Nil is
	// the default set; an empty list requires none.
	Required []string
}

// PressureLimit is one input's bounds; nil keeps the default. For
// free_storage_pct lower is worse, so slow is above stop.
type PressureLimit struct {
	Slow *float64
	Stop *float64
}

// The pressure inputs, by their wire names.
const (
	PressureReplicaLag   = "replica_lag_seconds"
	PressureXIDAge       = "xid_age"
	PressureMultixactAge = "multixact_age"
	PressureVacuum       = "vacuum_running"
	PressureWALRate      = "wal_bytes_per_second"
	PressureLockQueue    = "lock_queue"
	PressureFreeStorage  = "free_storage_pct"
)

// PressureInputs are the inputs in their fixed answer order.
func PressureInputs() []string {
	return []string{PressureReplicaLag, PressureXIDAge, PressureMultixactAge,
		PressureVacuum, PressureWALRate, PressureLockQueue, PressureFreeStorage}
}

// PressureLowerIsWorse reports an input whose low values are the risk.
func PressureLowerIsWorse(name string) bool { return name == PressureFreeStorage }

func (p PressurePolicy) isDefault() bool { return p.Limits == nil && p.Required == nil }

type pressureWire struct {
	Limits   map[string]json.RawMessage `json:"limits,omitempty"`
	Required []string                   `json:"required"`
}

type pressureLimitWire struct {
	Slow *float64 `json:"slow,omitempty"`
	Stop *float64 `json:"stop,omitempty"`
}

// pressureFromWire reads the pressure block (absent: the defaults).
func pressureFromWire(raw json.RawMessage) (PressurePolicy, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return PressurePolicy{}, nil
	}
	var wire pressureWire
	if err := decodeStrict(raw, &wire); err != nil {
		return PressurePolicy{}, fmt.Errorf("pressure: %w", err)
	}
	p := PressurePolicy{Required: wire.Required}
	if wire.Limits != nil {
		p.Limits = make(map[string]PressureLimit, len(wire.Limits))
	}
	for name, raw := range wire.Limits {
		var lw pressureLimitWire
		if err := decodeStrict(raw, &lw); err != nil {
			return PressurePolicy{}, fmt.Errorf("pressure.limits.%s: %w", name, err)
		}
		p.Limits[name] = PressureLimit(lw)
	}
	return p, ValidatePressurePolicy(p)
}

// ValidatePressurePolicy checks names, ranges and that each slow bound
// comes before its stop bound.
func ValidatePressurePolicy(p PressurePolicy) error {
	inputs := PressureInputs()
	for name, l := range p.Limits {
		if !slices.Contains(inputs, name) {
			return fmt.Errorf("pressure.limits.%s is not an input; inputs are %s", name,
				strings.Join(inputs, ", "))
		}
		if err := validatePressureLimit(name, l); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, name := range p.Required {
		if !slices.Contains(inputs, name) {
			return fmt.Errorf("pressure.required %q is not an input; inputs are %s", name,
				strings.Join(inputs, ", "))
		}
		if seen[name] {
			return fmt.Errorf("pressure.required names %q twice", name)
		}
		seen[name] = true
	}
	return nil
}

func validatePressureLimit(name string, l PressureLimit) error {
	if l.Slow == nil && l.Stop == nil {
		return fmt.Errorf("pressure.limits.%s sets neither slow nor stop; leave it out "+
			"for the defaults", name)
	}
	for bound, v := range map[string]*float64{"slow": l.Slow, "stop": l.Stop} {
		switch {
		case v == nil:
		case math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0:
			return fmt.Errorf("pressure.limits.%s.%s must be a finite value >= 0", name, bound)
		case PressureLowerIsWorse(name) && *v > 100:
			return fmt.Errorf("pressure.limits.%s.%s is a percentage, at most 100", name,
				bound)
		}
	}
	if l.Slow == nil || l.Stop == nil {
		return nil
	}
	if PressureLowerIsWorse(name) && *l.Slow <= *l.Stop {
		return fmt.Errorf("pressure.limits.%s.slow %v must be above stop %v (lower is "+
			"worse)", name, *l.Slow, *l.Stop)
	}
	if !PressureLowerIsWorse(name) && *l.Slow >= *l.Stop {
		return fmt.Errorf("pressure.limits.%s.slow %v must be below stop %v", name,
			*l.Slow, *l.Stop)
	}
	return nil
}

// pressureJSON is the pressure block MarshalDocument writes.
func pressureJSON(p PressurePolicy) map[string]any {
	out := map[string]any{}
	if p.Limits != nil {
		limits := make(map[string]pressureLimitWire, len(p.Limits))
		for name, l := range p.Limits {
			limits[name] = pressureLimitWire(l)
		}
		out["limits"] = limits
	}
	if p.Required != nil {
		out["required"] = p.Required
	}
	return out
}
