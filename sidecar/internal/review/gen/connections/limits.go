package connections

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Connection limits: under LimitHeadroom of usable connections free at
// the window's peak, a role whose peak is over its fair share (usable
// connections over the roles seen) is limited to its peak plus
// LimitMargin, capped at the usable connections, and never below its peak
// plus the role-budget contract's margin (budget.ConnectionLimitMargin).
const (
	LimitHeadroom = 0.10
	LimitMargin   = 0.20
)

// limits proposes a connection limit for each role over its fair share
// while headroom is short. Unknown headroom is a missing input.
func (b *builder) limits() {
	if len(b.in.RolePeaks) == 0 {
		return
	}
	if !b.v.measured {
		b.cov.AddMissing("max_connections")
		return
	}
	usable, peak := b.v.usable, b.v.peak
	// Headroom under 10%, in integers: 10 * (usable - peak) < usable.
	if 10*(usable-peak) >= usable {
		return
	}
	roles := 0
	for _, r := range b.in.RolePeaks {
		if r.Sessions > 0 {
			roles++
		}
	}
	fair := float64(usable) / float64(roles)
	for _, r := range b.in.RolePeaks {
		if float64(r.Sessions) > fair {
			b.limit(r, fair)
		}
	}
}

// proposedLimit is the role's limit from its peak: the peak plus
// LimitMargin capped at usable, raised to the peak plus the contract's
// margin, so it never rejects the sessions the role was seen holding.
func proposedLimit(rolePeak, usable int) int {
	limit := min(int(math.Ceil(float64(rolePeak)*(1+LimitMargin))), usable)
	return max(limit, rolePeak+budget.ConnectionLimitMargin(rolePeak))
}

// limit is the role's step, or advice when its facts were not read;
// nothing when the contract refuses the role or it is already limited at
// the proposal or tighter.
func (b *builder) limit(r RolePeak, fair float64) {
	limit := proposedLimit(r.Sessions, b.v.usable)
	headroom := math.Round(b.v.headroomPct()*10) / 10
	ev := []string{b.v.hour(r.At)}
	if b.v.maxConnID != "" {
		ev = append(ev, b.v.maxConnID)
	}
	c := candidate.Candidate{Kind: KindConnectionLimit, Target: roleTarget(r.Role),
		ActionType: candidate.ActionAdvice, EvidenceIDs: ev,
		Prediction: candidate.Prediction{Metric: "headroom_at_peak", Direction: "increase",
			Source: "workload_sessions cluster sessions by role and the settings measures",
			Quantities: map[string]float64{QuantityLimit: float64(limit),
				QuantityRolePeak: float64(r.Sessions), QuantityFairShare: math.Round(fair*10) / 10,
				QuantityHeadroomPct: headroom}}}
	reason := fmt.Sprintf("At the window's peak %d of %d usable connections were in use "+
		"(%.1f%% free, under %.0f%%). Role %s held %d sessions at its peak, over its fair "+
		"share of %.1f; a CONNECTION LIMIT of %d (its peak plus %.0f%%, never below the "+
		"peak) keeps it from taking the last slots from every other role.", b.v.peak,
		b.v.usable, headroom, LimitHeadroom*100, r.Role, r.Sessions, fair, limit,
		LimitMargin*100)
	fact, ok, why := b.roleFact(r.Role, "connection limit")
	switch {
	case why != "":
		return
	case !ok:
		c.Reason = reason + " pg_sage could not read the role, so this is advice, not a step."
		b.emit(c)
		return
	case fact.ConnLimit >= 0 && fact.ConnLimit <= limit:
		return
	}
	c.ActionType, c.Reason = ActionConnectionLimit, reason
	c.ForwardSQL = budget.ConnectionLimitSQL(r.Role, limit)
	c.RollbackSQL = budget.ConnectionLimitSQL(r.Role, fact.ConnLimit)
	b.emit(c)
}
