package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// MethodLadderFleet is tier 4's method: a member's measured effect in a
// staggered fleet rollout (lab-03b).
const MethodLadderFleet = "fleet_rollout"

// FleetEffect is one fleet member's measurement in a staggered rollout:
// its effect in percent of blocks per call (nil: no v2 number, the
// verdict flag alone decided), the untreated members' median change it
// was taken net of (nil: none), how it was measured, and the halt bar.
type FleetEffect struct {
	Database   string
	ActionID   int64
	EffectPct  *float64
	ControlPct *float64
	Method     string
	LimitPct   float64
	Failed     bool
	Detail     string
}

// FromFleetEffect is the tier-4 record of a member's fleet effect: a
// failed verdict or an effect above the halt bar failed, an effect at or
// under it passed, and no number is inconclusive (never a pass).
func FromFleetEffect(f FleetEffect, at time.Time) LadderRecord {
	r := newRecord(TierFleet, MethodLadderFleet, LadderPassed, at)
	r.Evidence["database"] = f.Database
	r.Evidence["action_id"] = f.ActionID
	r.Evidence["limit_pct"] = f.LimitPct
	r.Evidence["method"] = f.Method
	if f.ControlPct != nil {
		r.Evidence["control_pct"] = *f.ControlPct
	}
	if f.Detail != "" {
		r.Evidence["detail"] = f.Detail
	}
	if f.EffectPct != nil {
		effect := *f.EffectPct
		r.Predicted, r.Metric = &effect, LadderMetricBlocksPerCall
	}
	switch {
	case f.Failed || (f.EffectPct != nil && *f.EffectPct > f.LimitPct):
		r.Result = LadderFailed
		return r
	case f.EffectPct == nil:
		return inconclusive(r, "no measured effect: the verdict flag alone decided")
	}
	return r
}

// RecordFleetForAction sets a member's tier-4 record on the ladder of the
// queue rows that ran its action, replacing an earlier fleet record. It
// reports how many rows it updated (0: the action ran without a queue
// item, as a canary's operator-approved execution does).
func RecordFleetForAction(ctx context.Context, db LadderDB, actionID int64,
	rec LadderRecord) (int64, error) {
	rec.Method = MethodLadderFleet
	raw, err := json.Marshal([]LadderRecord{rec})
	if err != nil {
		return 0, fmt.Errorf("encode the fleet record of action %d: %w", actionID, err)
	}
	tag, err := db.Exec(ctx, recordVerdictForActionSQL, actionID, raw, MethodLadderFleet)
	if err != nil {
		return 0, fmt.Errorf("record the tier-4 fleet effect of action %d: %w", actionID, err)
	}
	return tag.RowsAffected(), nil
}
