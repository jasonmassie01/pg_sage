package policy

import (
	"fmt"
	"slices"
	"strings"
)

// DrillPolicy is the standing policy's drill block (drill-01a): whether
// the database gets restore drills, how often, on which clone kinds, how
// long one may take and the operator's latest-committed-marker query. A
// zero field means its default, so a document without the block gets
// drills on cheap clone kinds every 30 days.
type DrillPolicy struct {
	// Enabled is nil for the default (on).
	Enabled *bool
	// CadenceDays is days between drills (1-365, default 30).
	CadenceDays int64
	// Kinds are the clone kinds a drill may use: thin_clone, branch,
	// copy_on_write, full_copy. Nil is the cheap three; full_copy also needs
	// clone.allow_full_copy.
	Kinds []string
	// MaxDurationMinutes bounds one drill (1-1440, default 240).
	MaxDurationMinutes int64
	// MarkerQuery is one SELECT (or WITH ... SELECT) returning the newest
	// committed timestamp, run on the clone only, when the sage schema is
	// not in the database.
	MarkerQuery string
}

// The drill block's defaults and ranges.
const (
	DefaultDrillCadenceDays        = 30
	DefaultDrillMaxDurationMinutes = 240
	maxDrillCadenceDays            = 365
	maxDrillDurationMinutes        = 1440
	maxDrillMarkerQuery            = 2000
)

// drillKinds are the clone kinds a drill block may name.
var drillKinds = []string{"thin_clone", "branch", "copy_on_write", "full_copy"}

// On reports whether drills run (the default).
func (d DrillPolicy) On() bool { return d.Enabled == nil || *d.Enabled }

// Resolved is d with every zero field set to its default (Kinds stays nil:
// the caller's default kinds).
func (d DrillPolicy) Resolved() DrillPolicy {
	if d.CadenceDays == 0 {
		d.CadenceDays = DefaultDrillCadenceDays
	}
	if d.MaxDurationMinutes == 0 {
		d.MaxDurationMinutes = DefaultDrillMaxDurationMinutes
	}
	d.Kinds = slices.Clone(d.Kinds)
	return d
}

func (d DrillPolicy) isDefault() bool {
	return d.Enabled == nil && d.CadenceDays == 0 && d.Kinds == nil &&
		d.MaxDurationMinutes == 0 && d.MarkerQuery == ""
}

// ValidateDrillPolicy checks d's ranges; zero fields are the defaults.
func ValidateDrillPolicy(d DrillPolicy) error {
	if v := d.CadenceDays; v != 0 && (v < 1 || v > maxDrillCadenceDays) {
		return fmt.Errorf("drill.cadence_days %d must be 1-%d", v, maxDrillCadenceDays)
	}
	if v := d.MaxDurationMinutes; v != 0 && (v < 1 || v > maxDrillDurationMinutes) {
		return fmt.Errorf("drill.max_duration_minutes %d must be 1-%d", v,
			maxDrillDurationMinutes)
	}
	if d.Kinds != nil && len(d.Kinds) == 0 {
		return fmt.Errorf("drill.kinds cannot be empty; set drill.enabled false to stop " +
			"drills")
	}
	for _, k := range d.Kinds {
		if !slices.Contains(drillKinds, k) {
			return fmt.Errorf("drill.kinds %q must be one of %s", k,
				strings.Join(drillKinds, ", "))
		}
	}
	return validateMarkerQuery(d.MarkerQuery)
}

// validateMarkerQuery accepts one SELECT or WITH statement; it runs on
// the clone only, inside a READ ONLY transaction with a statement timeout.
func validateMarkerQuery(q string) error {
	if q == "" {
		return nil
	}
	trimmed := strings.TrimSpace(q)
	lower := strings.ToLower(trimmed)
	switch {
	case trimmed == "":
		return fmt.Errorf("drill.marker_query cannot be blank; leave it out for none")
	case len(q) > maxDrillMarkerQuery:
		return fmt.Errorf("drill.marker_query is longer than %d characters",
			maxDrillMarkerQuery)
	case strings.Contains(trimmed, ";"):
		return fmt.Errorf("drill.marker_query must be one statement, without ';'")
	case !strings.HasPrefix(lower, "select") && !strings.HasPrefix(lower, "with"):
		return fmt.Errorf("drill.marker_query must be a SELECT (or WITH ... SELECT) that " +
			"returns one timestamp")
	}
	return nil
}

// drillWire is the drill block on the wire. Pointers tell an explicit
// value (which must be in range: 0 is refused) from an absent one.
type drillWire struct {
	Enabled            *bool    `json:"enabled,omitempty"`
	CadenceDays        *int64   `json:"cadence_days,omitempty"`
	Kinds              []string `json:"kinds"`
	MaxDurationMinutes *int64   `json:"max_duration_minutes,omitempty"`
	MarkerQuery        *string  `json:"marker_query,omitempty"`
}

// drillFromWire reads the drill block (nil: the defaults).
func drillFromWire(wire *drillWire) (DrillPolicy, error) {
	if wire == nil {
		return DrillPolicy{}, nil
	}
	d := DrillPolicy{Enabled: wire.Enabled, Kinds: wire.Kinds}
	if wire.CadenceDays != nil {
		if *wire.CadenceDays == 0 {
			return DrillPolicy{}, fmt.Errorf("drill.cadence_days cannot be 0; leave it out " +
				"for the default")
		}
		d.CadenceDays = *wire.CadenceDays
	}
	if wire.MaxDurationMinutes != nil {
		if *wire.MaxDurationMinutes == 0 {
			return DrillPolicy{}, fmt.Errorf("drill.max_duration_minutes cannot be 0; " +
				"leave it out for the default")
		}
		d.MaxDurationMinutes = *wire.MaxDurationMinutes
	}
	if wire.MarkerQuery != nil {
		if strings.TrimSpace(*wire.MarkerQuery) == "" {
			return DrillPolicy{}, fmt.Errorf("drill.marker_query cannot be blank; leave " +
				"it out for none")
		}
		d.MarkerQuery = *wire.MarkerQuery
	}
	return d, ValidateDrillPolicy(d)
}

// drillJSON is the drill block MarshalDocument writes.
func drillJSON(d DrillPolicy) map[string]any {
	r := d.Resolved()
	out := map[string]any{"enabled": r.On(), "cadence_days": r.CadenceDays,
		"max_duration_minutes": r.MaxDurationMinutes}
	if r.Kinds != nil {
		out["kinds"] = r.Kinds
	}
	if r.MarkerQuery != "" {
		out["marker_query"] = r.MarkerQuery
	}
	return out
}
