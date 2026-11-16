package optimizer

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// IndexRehearser rehearses an admitted index create on a disposable clone
// and returns its tier-2 ladder record (lab-04a). It is declared here
// because tuning imports optimizer; the sidecar adapts tuning's
// rehearser to it. A failure is a skipped record, never a pass.
type IndexRehearser interface {
	RehearseIndex(ctx context.Context, ddl string, queries []QueryInfo) verify.LadderRecord
}

// floorRehearseMax bounds the statements one floor rehearsal measures.
const floorRehearseMax = 5

// Floor tier-2 skip reasons.
const (
	floorNoCloneProvider = "no clone provider"
	floorNoTargets       = "no target statement to rehearse"
)

// WithRehearser makes the floor rehearse each admitted candidate on a
// clone before it is proposed; nil keeps the floor plan-only.
func (f *Floor) WithRehearser(r IndexRehearser) *Floor {
	if f != nil {
		f.reh = r
	}
	return f
}

// rehearse attaches the candidate's tier-2 record: the rehearsal's, or a
// skip saying why there was none. The candidate is proposed either way
// (recommend-only behaviour is unchanged).
func (f *Floor) rehearse(ctx context.Context, adm *Admission, tc TableContext) {
	now := time.Now()
	queries := floorTargets(adm.Rec, tc)
	var rec verify.LadderRecord
	switch {
	case f.reh == nil:
		rec = verify.LadderBuildError(verify.TierClone, verify.MethodLadderRehearsal,
			errors.New(floorNoCloneProvider), now)
	case len(queries) == 0:
		rec = verify.LadderBuildError(verify.TierClone, verify.MethodLadderRehearsal,
			errors.New(floorNoTargets), now)
	default:
		rec = f.reh.RehearseIndex(ctx, adm.Rec.DDL, queries)
		f.logFn("INFO", "optimizer: index floor: %s rehearsed on a clone: %s",
			adm.Rec.DDL, rec.Result)
	}
	adm.Rec.Ladder = append(adm.Rec.Ladder, rec)
}

// floorTargets are the statements a rehearsal measures: the candidate's
// affected statements when it names them, else the table's, at most
// floorRehearseMax in the context's order.
func floorTargets(rec Recommendation, tc TableContext) []QueryInfo {
	var out []QueryInfo
	for _, q := range tc.Queries {
		if len(out) == floorRehearseMax {
			break
		}
		if len(rec.AffectedQueryIDs) == 0 || slices.Contains(rec.AffectedQueryIDs, q.QueryID) {
			out = append(out, q)
		}
	}
	return out
}
