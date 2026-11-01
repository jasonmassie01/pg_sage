package probes

import (
	"errors"
	"fmt"
	"time"
)

// Signal probes (M5) are evidence sources outside the SQL catalog: the
// change feed ("what changed?") and the SLO status ("are customers
// hurt?"), and an external pooler's telemetry (pooler.go). They are
// produced in Go, typed like every probe result and never offered to the
// model as catalog probes.
const (
	ChangeFeed ID = "change_feed"
	SLOStatus  ID = "slo_status"
	// ApplicationSLOStatus is only a registered database's explicitly bound
	// pushed application SLI; it cannot include Prometheus or database proxies.
	ApplicationSLOStatus ID = "application_slo_status"
)

// IsSignal reports whether id is a signal probe.
func IsSignal(id ID) bool {
	return id == ChangeFeed || id == SLOStatus || id == ApplicationSLOStatus || id == PoolerPools
}

// ChangeRow is one change_feed row: a change event in the window.
type ChangeRow struct {
	Kind       string
	Source     string
	Summary    string
	Signature  string
	EventID    string
	Service    string
	OccurredAt time.Time
	AgeS       float64
}

// ChangeRows decodes a change_feed result.
func ChangeRows(res Result) ([]ChangeRow, error) {
	rows, err := rowsFor(res, ChangeFeed)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]ChangeRow, 0, len(rows))
	for i, r := range rows {
		c := ChangeRow{Kind: strField(r, "kind"), Source: strField(r, "source"),
			Summary: strField(r, "summary"), Signature: strField(r, "signature"),
			EventID: strField(r, "event_id"), Service: strField(r, "service"),
			OccurredAt: timeField(r, "occurred_at"), AgeS: floatField(r, "age_s")}
		if c.Kind == "" {
			return nil, fmt.Errorf("change_feed row %d: %w", i+1, errNoKey("kind"))
		}
		out = append(out, c)
	}
	return out, nil
}

// SLORow is one slo_status row: one SLO's error-budget state. Unknown
// numbers (burn rates, budget) decode as NaN.
type SLORow struct {
	Name            string
	Source          string
	Kind            string
	State           string
	Unknown         string
	FastBurning     bool
	CustomerImpact  bool
	BurnLong        float64
	BurnShort       float64
	LongWindow      string
	ShortWindow     string
	BudgetRemaining float64
	EvaluatedAt     time.Time
	AgeS            float64
}

// SLORows decodes an slo_status result.
func SLORows(res Result) ([]SLORow, error) {
	id := SLOStatus
	if res.ProbeID == ApplicationSLOStatus {
		id = ApplicationSLOStatus
	}
	rows, err := rowsFor(res, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]SLORow, 0, len(rows))
	for i, r := range rows {
		s := SLORow{Name: strField(r, "name"), Kind: strField(r, "kind"),
			Source: strField(r, "source"),
			State:  strField(r, "state"), Unknown: strField(r, "unknown"),
			FastBurning:    boolField(r, "fast_burning"),
			CustomerImpact: boolField(r, "customer_impact"),
			BurnLong:       floatField(r, "burn_long"), BurnShort: floatField(r, "burn_short"),
			LongWindow: strField(r, "long_window"), ShortWindow: strField(r, "short_window"),
			BudgetRemaining: floatField(r, "budget_remaining"),
			EvaluatedAt:     timeField(r, "evaluated_at"), AgeS: floatField(r, "age_s")}
		if s.Name == "" {
			return nil, fmt.Errorf("slo_status row %d: %w", i+1, errNoKey("name"))
		}
		if id == ApplicationSLOStatus && (s.Kind != "app" || s.Source != "push") {
			return nil, fmt.Errorf("application_slo_status row %d is not a bound pushed application SLI", i+1)
		}
		out = append(out, s)
	}
	return out, nil
}

func errNoKey(key string) error { return errors.New("missing " + key) }
