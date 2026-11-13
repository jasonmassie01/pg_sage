package freeze

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func EvaluateHorizon(
	now time.Time, sample HorizonSample, thresholds Thresholds,
) (Assessment, error) {
	if err := validateSample(sample, thresholds); err != nil {
		return Assessment{}, err
	}
	xid := horizon(now, sample.XIDAge, sample.XIDMaxAge,
		sample.XIDsPerSecond, thresholds)
	multi := horizon(now, sample.MultiXactAge, sample.MultiXactMaxAge,
		sample.MultiXactsPerSecond, thresholds)
	threat, selected := ThreatXID, xid
	if multi.HardAt.Before(xid.HardAt) {
		threat, selected = ThreatMultiXact, multi
	}
	deadline := Deadline{Kind: DeadlineXID, Threat: threat, Urgency: selected.Urgency,
		HardAt: selected.HardAt, Remaining: selected.Distance}
	proposal := Proposal{}
	urgency := maxUrgency(xid.Urgency, multi.Urgency)
	if urgency != UrgencyGreen {
		proposal = freezeProposal(sample, threat, urgency, deadline)
	}
	return Assessment{XID: xid, MultiXact: multi, Urgency: urgency,
		Deadline: deadline, Proposal: proposal}, nil
}

func validateSample(sample HorizonSample, thresholds Thresholds) error {
	if strings.TrimSpace(sample.Database) == "" || strings.TrimSpace(sample.Schema) == "" ||
		strings.TrimSpace(sample.Table) == "" {
		return fmt.Errorf("table identity is required")
	}
	if sample.XIDMaxAge <= 0 {
		return fmt.Errorf("xid maximum must be positive")
	}
	if !usableRate(sample.XIDsPerSecond) {
		return fmt.Errorf("xid rate must be positive and finite, got %v", sample.XIDsPerSecond)
	}
	if sample.MultiXactMaxAge <= 0 {
		return fmt.Errorf("multixact maximum must be positive")
	}
	if !usableRate(sample.MultiXactsPerSecond) {
		return fmt.Errorf("multixact rate must be positive and finite, got %v",
			sample.MultiXactsPerSecond)
	}
	if thresholds.RedBufferPct <= 0 || thresholds.AmberBufferPct <= thresholds.RedBufferPct ||
		thresholds.AmberBufferPct > 100 {
		return fmt.Errorf("invalid urgency thresholds")
	}
	return nil
}

// usableRate rejects zero, negative, NaN and infinite consumption rates.
func usableRate(rate float64) bool { return rate > 0 && !math.IsInf(rate, 1) }

// maxDeadlineSeconds caps a deadline at 100 years, far inside
// time.Duration's ~292-year range.
const maxDeadlineSeconds = 100 * 365.25 * 24 * 60 * 60

// horizon judges one counter. Urgency (and Distance) are relative to the
// freeze maximum; the hard deadline is when the age reaches
// WraparoundWarnAge at the rate. At or past that limit the server is
// already warning: the urgency is red and the deadline is when the age
// reaches WraparoundStopAge, so it stays ahead while the scan's other
// proposals run first.
func horizon(now time.Time, age, maximum int64, rate float64, thresholds Thresholds) Horizon {
	distance := maximum - age
	result := Horizon{Distance: distance, Urgency: urgency(distance, maximum, thresholds)}
	limit := int64(WraparoundWarnAge)
	if age >= WraparoundWarnAge {
		limit, result.Urgency = WraparoundStopAge, UrgencyRed
	}
	result.HardAt = runwayDeadline(now, limit-age, rate)
	return result
}

// runwayDeadline is when distance more IDs are consumed at rate, capped at
// 100 years; with no distance left it is one second ahead.
func runwayDeadline(now time.Time, distance int64, rate float64) time.Time {
	if distance <= 0 {
		return now.Add(time.Second)
	}
	seconds := math.Min(math.Ceil(float64(distance)/rate), maxDeadlineSeconds)
	return now.Add(time.Duration(seconds) * time.Second)
}

func urgency(distance, maximum int64, thresholds Thresholds) Urgency {
	if float64(distance) <= float64(maximum)*thresholds.RedBufferPct/100 {
		return UrgencyRed
	}
	if float64(distance) <= float64(maximum)*thresholds.AmberBufferPct/100 {
		return UrgencyAmber
	}
	return UrgencyGreen
}

// BufferThresholds derives the urgency buffers from the configured red
// percentage: amber is twice red, capped at 100. Config validation admits
// only 0 < red < 100; a value outside that, NaN included, falls back to
// the 25% default for a caller that skipped validation.
func BufferThresholds(redPct float64) Thresholds {
	if !(redPct > 0 && redPct < 100) {
		redPct = 25
	}
	return Thresholds{RedBufferPct: redPct, AmberBufferPct: math.Min(redPct*2, 100)}
}

// TableUrgency is the worse of a table's XID and multixact urgency. A
// non-positive maximum reads as red, so an unmeasured horizon never looks
// safe.
func TableUrgency(
	xidAge, xidMax, mxidAge, mxidMax int64, thresholds Thresholds,
) Urgency {
	return maxUrgency(urgency(xidMax-xidAge, xidMax, thresholds),
		urgency(mxidMax-mxidAge, mxidMax, thresholds))
}

func maxUrgency(left, right Urgency) Urgency {
	if left == UrgencyRed || right == UrgencyRed {
		return UrgencyRed
	}
	if left == UrgencyAmber || right == UrgencyAmber {
		return UrgencyAmber
	}
	return UrgencyGreen
}

func freezeProposal(
	sample HorizonSample, threat ThreatKind, urgency Urgency, deadline Deadline,
) Proposal {
	return Proposal{Database: sample.Database, Schema: sample.Schema, Table: sample.Table,
		Threat: threat, Urgency: urgency, Intent: IntentVacuumFreeze, Deadline: deadline}
}

func quoteIdent(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
