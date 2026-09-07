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
	if sample.XIDsPerSecond <= 0 {
		return fmt.Errorf("xid rate must be positive")
	}
	if sample.MultiXactMaxAge <= 0 {
		return fmt.Errorf("multixact maximum must be positive")
	}
	if sample.MultiXactsPerSecond <= 0 {
		return fmt.Errorf("multixact rate must be positive")
	}
	if thresholds.RedBufferPct <= 0 || thresholds.AmberBufferPct <= thresholds.RedBufferPct ||
		thresholds.AmberBufferPct > 100 {
		return fmt.Errorf("invalid urgency thresholds")
	}
	return nil
}

func horizon(now time.Time, age, maximum int64, rate float64, thresholds Thresholds) Horizon {
	distance := maximum - age
	seconds := float64(distance) / rate
	if seconds < 0 {
		seconds = 0
	}
	return Horizon{Distance: distance,
		HardAt:  now.Add(time.Duration(math.Ceil(seconds) * float64(time.Second))),
		Urgency: urgency(distance, maximum, thresholds)}
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
		Threat: threat, Urgency: urgency, Intent: IntentVacuumFreeze,
		SQL:      "VACUUM (FREEZE) " + quoteIdent(sample.Schema) + "." + quoteIdent(sample.Table),
		Deadline: deadline, RequiresAuthorization: true}
}

func quoteIdent(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
