package cases

import "fmt"

// Urgency is how soon a case's condition reaches its impact, read only
// from the fields its producer wrote (cases-07): the forecast lane owns
// the numbers and a case never derives one of its own. A needed field
// that is missing, not a number, not finite or not positive leaves the
// case unassessed: an unknown is never shown as a reassuring number.
type Urgency struct {
	Assessed            bool     `json:"assessed"`
	TimeToImpactSeconds *float64 `json:"time_to_impact_seconds,omitempty"`
	// Impact names what happens at the time to impact ("until the disk
	// is full").
	Impact string `json:"impact,omitempty"`
	// Basis is the formula behind the number, or the measured headroom
	// of an unassessed case; it is always shown, never hidden.
	Basis  string         `json:"basis,omitempty"`
	Inputs map[string]any `json:"inputs,omitempty"`
	// LimitReached is set when the producer reports a zero time to its
	// limit: nothing is left to project, so the case stays unassessed and
	// shows no number, yet it ranks before every projection.
	LimitReached bool `json:"limit_reached,omitempty"`
}

// minReliableR2 is the default of forecaster.min_r_squared, the fit the
// forecaster calls reliable.
const minReliableR2 = 0.5

// urgencyRead is one category's reading: the urgency and its why-now.
type urgencyRead struct {
	urgency Urgency
	whyNow  string
}

type urgencyRule func(detail map[string]any) urgencyRead

// urgencyRules is the one table of producers whose fields urgency reads.
var urgencyRules = map[string]urgencyRule{
	"forecast_disk_growth":             storageUrgency,
	"forecast_connection_saturation":   connectionUrgency,
	"forecast_wal_runway":              runwayUrgency,
	"forecast_sequence_runway":         runwayUrgency,
	"forecast_wraparound_runway":       runwayUrgency,
	"forecast_multixact_member_runway": runwayUrgency,
	"xid_wraparound":                   xidHeadroom,
	"sequence_exhaustion":              sequenceHeadroom,
}

// UrgencyForFinding is the finding's urgency, from its category's rule.
func UrgencyForFinding(f SourceFinding) Urgency {
	return readUrgency(f).urgency
}

func readUrgency(f SourceFinding) urgencyRead {
	rule, ok := urgencyRules[f.Category]
	if !ok {
		return urgencyRead{whyNow: notAssessed}
	}
	return rule(f.Detail)
}

const notAssessed = "Urgency not assessed"

// unassessed is a reading whose named field gave no positive projection;
// the sentence names the field, never its value.
func unassessed(field, why string) urgencyRead {
	return urgencyRead{whyNow: fmt.Sprintf("%s: %s %s", notAssessed, field, why)}
}

// unassessedOrReached is the reading of a field that gave no positive
// projection: a producer-written zero means the limit is already reached.
func unassessedOrReached(d map[string]any, field, why string, keys ...string) urgencyRead {
	if v, ok := finiteField(d, field); !ok || v != 0 {
		return unassessed(field, why)
	}
	return urgencyRead{urgency: Urgency{LimitReached: true, Inputs: pick(d, keys...)},
		whyNow: fmt.Sprintf("%s: %s is zero, so the producer reports the limit "+
			"already reached", notAssessed, field)}
}

// assessed is a reading with a positive, finite time to impact.
func assessed(secs float64, impact, basis string, inputs map[string]any) urgencyRead {
	u := Urgency{Assessed: true, TimeToImpactSeconds: &secs, Impact: impact,
		Basis: basis, Inputs: inputs}
	return urgencyRead{urgency: u,
		whyNow: fmt.Sprintf("About %s %s (%s)", HumanDuration(secs), impact, basis)}
}

// storageUrgency reads forecast_disk_growth as forecast-06 writes it: the
// days until the database fills its known capacity, from a regression over
// covered_days closed days.
func storageUrgency(d map[string]any) urgencyRead {
	days, ok := positiveField(d, "days_until_full")
	if !ok {
		return unassessedOrReached(d, "days_until_full", "is not a positive projection",
			"days_until_full", "current_size_bytes", "capacity_bytes", "capacity_source")
	}
	r2, ok := finiteField(d, "r_squared")
	if !ok || r2 < minReliableR2 {
		return unassessed("r_squared", "is below the forecaster's reliable fit")
	}
	basis := fmt.Sprintf("days_until_full %s from a regression, R² %.2f", plain(days), r2)
	if covered, ok := positiveField(d, "covered_days"); ok {
		basis = fmt.Sprintf("days_until_full %s from a %s-day regression, R² %.2f",
			plain(days), plain(covered), r2)
	}
	return assessed(days*86400, "until storage is full", basis, pick(d,
		"days_until_full", "r_squared", "covered_days", "capacity_source",
		"capacity_bytes", "projected_full_date"))
}

func connectionUrgency(d map[string]any) urgencyRead {
	days, ok := positiveField(d, "days_to_warn_pct")
	if !ok {
		return unassessedOrReached(d, "days_to_warn_pct", "is not a positive projection",
			"days_to_warn_pct", "current_in_use", "max_connections")
	}
	r2, ok := finiteField(d, "r_squared")
	if !ok || r2 < minReliableR2 {
		return unassessed("r_squared", "is below the forecaster's reliable fit")
	}
	basis := fmt.Sprintf("days_to_warn_pct %s from a daily regression, R² %.2f", plain(days), r2)
	return assessed(days*86400, "until connections reach the warning share of max_connections",
		basis, pick(d, "days_to_warn_pct", "r_squared", "slope_per_day"))
}

// runwayImpact is what a runway kind reaches at its limit.
var runwayImpact = map[string]string{
	"disk_used":    "until the disk is full",
	"wal_slot":     "until the slot reaches its WAL maximum",
	"sequence":     "until the sequence reaches its limit",
	"xid":          "until the XID wraparound warning limit",
	"mxid":         "until the multixact wraparound warning limit",
	"mxid_members": "until aggressive vacuuming of multixact member space starts",
}

func runwayUrgency(d map[string]any) urgencyRead {
	if kind, _ := d["runway_kind"].(string); kind == "overdue_table" {
		return overdueHeadroom(d)
	}
	secs, ok := positiveField(d, "seconds_to_limit")
	if !ok {
		return unassessedOrReached(d, "seconds_to_limit", "is not a positive projection",
			"seconds_to_limit", "limit", "last_value")
	}
	if _, ok := positiveField(d, "rate_per_s"); !ok {
		return unassessed("rate_per_s", "is not a positive measured rate")
	}
	impact, ok := runwayImpact[stringField(d, "runway_kind")]
	if !ok {
		impact = "until its limit is reached"
	}
	return assessed(secs, impact, runwayBasis(d, secs), pick(d, "seconds_to_limit",
		"rate_per_s", "samples", "span_s", "r2", "limit", "last_value"))
}

func runwayBasis(d map[string]any, secs float64) string {
	basis := fmt.Sprintf("seconds_to_limit %.0f", secs)
	if n, ok := positiveField(d, "samples"); ok {
		basis += fmt.Sprintf(" from %s samples", plain(n))
	}
	if span, ok := positiveField(d, "span_s"); ok {
		basis += " over " + compactDuration(span)
	}
	if r2, ok := finiteField(d, "r2"); ok {
		return basis + fmt.Sprintf(", R² %.2f", r2)
	}
	return basis + ", R² not measured"
}
