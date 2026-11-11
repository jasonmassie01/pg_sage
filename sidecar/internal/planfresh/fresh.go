// Package planfresh decides whether a cached plan still describes how
// PostgreSQL runs a statement (tuning-08). The query tuner and the tuning
// agent share it: a plan older than tuner.plan_max_age_minutes, than the
// last ANALYZE of a relation it reads, or than pg_sage's last executed
// action on one of those relations is stale and never drives a hint.
package planfresh

import (
	"bytes"
	"fmt"
	"time"
)

// DefaultMaxAge is the age limit when tuner.plan_max_age_minutes is unset.
const DefaultMaxAge = 24 * time.Hour

// MaxAge is the age limit for plan_max_age_minutes; 0 or less is the
// default, never "no limit".
func MaxAge(minutes int) time.Duration {
	if minutes <= 0 {
		return DefaultMaxAge
	}
	return time.Duration(minutes) * time.Minute
}

// Capture is one sage.explain_cache row.
type Capture struct {
	JSON       []byte
	Source     string // the row's source (explain_estimate, auto_explain_log, ...)
	CapturedAt time.Time
	// AnalyzeEnabled is the provider's flag; nil means unknown.
	AnalyzeEnabled *bool
}

// Events are what happened to the plan's relations, read from the catalog
// and pg_sage's action log. A zero time means none is known.
type Events struct {
	Analyzed time.Time // latest last_analyze / last_autoanalyze
	Action   time.Time // latest executed (not failed) pg_sage action
}

// Verdict says whether a capture may be used and, when not, why.
type Verdict struct {
	Fresh  bool
	Reason string
}

func stale(format string, args ...any) Verdict {
	return Verdict{Reason: fmt.Sprintf(format, args...)}
}

// Fresh judges c at now. A capture at the same instant as an event cannot
// be shown to follow it, so it is stale (fail closed).
func Fresh(c Capture, ev Events, now time.Time, maxAge time.Duration) Verdict {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	if c.CapturedAt.IsZero() {
		return stale("the capture time is unknown")
	}
	if age := now.Sub(c.CapturedAt); age >= maxAge {
		return stale("captured %s ago, as old as or older than the %s limit",
			age.Round(time.Second), maxAge)
	}
	if !ev.Analyzed.IsZero() && !c.CapturedAt.After(ev.Analyzed) {
		return stale("captured before the last ANALYZE of a relation it reads (%s)",
			ev.Analyzed.UTC().Format(time.RFC3339))
	}
	if !ev.Action.IsZero() && !c.CapturedAt.After(ev.Action) {
		return stale("captured before a pg_sage action on a relation it reads (%s)",
			ev.Action.UTC().Format(time.RFC3339))
	}
	return Verdict{Fresh: true}
}

// actualsKeys appear only in EXPLAIN ANALYZE output.
var actualsKeys = [][]byte{[]byte(`"Actual Rows"`), []byte(`"Actual Loops"`),
	[]byte(`"Sort Space Type"`), []byte(`"Hash Batches"`)}

// HasActuals reports a capture of an executed plan: the provider says it
// was analyzed, or (provider unknown) the plan carries ANALYZE-only fields.
// A provider's explicit "not analyzed" wins over the fields.
func HasActuals(c Capture) bool {
	if c.AnalyzeEnabled != nil {
		return *c.AnalyzeEnabled
	}
	for _, k := range actualsKeys {
		if bytes.Contains(c.JSON, k) {
			return true
		}
	}
	return false
}
