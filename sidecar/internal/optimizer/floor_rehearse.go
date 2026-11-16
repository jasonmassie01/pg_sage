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
		rec = f.rehearseOnce(ctx, adm.Rec, queries, now)
	}
	adm.Rec.Ladder = append(adm.Rec.Ladder, rec)
}

// floorRehearsalReuse is how long the floor keeps a candidate's tier-2
// record: the floor re-judges its open candidates every cycle and a
// finding's detail is replaced on refresh, so a fresh record is reused
// rather than a new clone built each cycle (and, at the hourly cap, the
// result replaced by a "cap reached" skip). It matches the default
// clone.max_clone_age_minutes.
const floorRehearsalReuse = 24 * time.Hour

// rehearseOnce is the candidate's fresh remembered record, or a new
// rehearsal. A skip (the cap, a clone that could not be made) is not
// remembered, so the next cycle tries again.
func (f *Floor) rehearseOnce(ctx context.Context, rec Recommendation,
	queries []QueryInfo, now time.Time) verify.LadderRecord {
	key := rec.FindingIdentifier()
	f.mu.Lock()
	prior, ok := f.rehearsed[key]
	f.mu.Unlock()
	if ok && now.Sub(prior.At) < floorRehearsalReuse {
		return prior
	}
	got := f.reh.RehearseIndex(ctx, rec.DDL, queries)
	f.logFn("INFO", "optimizer: index floor: %s rehearsed on a clone: %s", rec.DDL,
		got.Result)
	if got.Result != verify.LadderSkipped {
		f.remember(key, got, now)
	}
	return got
}

// remember keeps rec for key and drops the records past their reuse.
func (f *Floor) remember(key string, rec verify.LadderRecord, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rehearsed == nil {
		f.rehearsed = map[string]verify.LadderRecord{}
	}
	for k, r := range f.rehearsed {
		if now.Sub(r.At) >= floorRehearsalReuse {
			delete(f.rehearsed, k)
		}
	}
	f.rehearsed[key] = rec
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
