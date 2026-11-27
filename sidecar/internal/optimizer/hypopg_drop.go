package optimizer

import (
	"context"
	"fmt"
)

// DropMeasurement is the what-if evidence for dropping an existing index:
// the workload planned with the index and again with it hidden (HypoPG
// 1.4+). Only a verified measurement carries a change.
type DropMeasurement struct {
	// Verdict is WhatIfVerified, WhatIfUnverified (not measured, or not
	// completely) or WhatIfRejected (a guarded statement regressed).
	Verdict string
	// Reason says why the verdict is not verified.
	Reason string
	// ChangePct is the call-weighted planned-cost change with the index
	// hidden, in percent: positive is slower. Zero unless verified.
	ChangePct float64
	// Measured counts the statements planned both ways; Guarded those
	// whose plan used the index.
	Measured int
	Guarded  int
	// Regressions are the guarded statements that rose more than
	// maxGuardedRegressionPct.
	Regressions []QueryRegression
}

// dropWhatIf is a what-if that can also measure a drop (HypoPG).
type dropWhatIf interface {
	MeasureDrop(ctx context.Context, ix IndexRef, queries []QueryInfo) (WhatIfResult, error)
}

// MeasureDrop measures dropping ix from tc's table: each statement of the
// table's workload is planned with ix and with ix hidden. A guarded
// statement (one whose plan used ix) that rises more than 10% rejects the
// drop; anything short of a complete measurement is unverified.
func (o *Optimizer) MeasureDrop(ctx context.Context, ix IndexRef, tc TableContext,
) DropMeasurement {
	unverified := func(reason string) DropMeasurement {
		return DropMeasurement{Verdict: WhatIfUnverified, Reason: reason}
	}
	dw, ok := o.whatIf.(dropWhatIf)
	if o.whatIf == nil || !ok || !o.whatIf.IsAvailable(ctx) {
		return unverified("HypoPG unavailable")
	}
	if ix.OID == 0 {
		return unverified(fmt.Sprintf("the identity (OID) of %s is unknown, so it cannot "+
			"be hidden", ix.Name))
	}
	res, err := dw.MeasureDrop(ctx, ix, tc.Queries)
	if err != nil {
		o.logFn("optimizer", "hypopg drop measurement failed for %s: %v", ix.Name, err)
	}
	verdict, reason := dropVerdict(res, err)
	m := DropMeasurement{Verdict: verdict, Reason: reason, Measured: res.Measured,
		Guarded: res.Guarded, Regressions: res.Regressions}
	if verdict == WhatIfVerified && res.Improvement != 0 {
		m.ChangePct = -res.Improvement
	}
	return m
}

// dropVerdict judges a drop measurement. A measured regression of a guarded
// statement rejects whatever else was planned; otherwise only a complete
// measurement verifies.
func dropVerdict(res WhatIfResult, err error) (string, string) {
	switch {
	case err != nil:
		return WhatIfUnverified, "drop what-if failed: " + err.Error()
	case res.OldIndexPresent:
		return WhatIfUnverified, dropUnsupportedReason
	case len(res.Regressions) > 0:
		return WhatIfRejected, regressionReason("dropping", res)
	case res.Measured == 0:
		return WhatIfUnverified, "no workload query could be measured"
	case res.Failed > 0:
		return WhatIfUnverified, partialReason(res)
	}
	return WhatIfVerified, ""
}

// MeasureDrop plans the table's workload (at most maxWhatIfStatements, the
// hottest first) with ix and with ix hidden, on one owning session. Without
// hypopg_hide_index nothing is measured and the result says so.
func (h *HypoPG) MeasureDrop(ctx context.Context, ix IndexRef, queries []QueryInfo,
) (WhatIfResult, error) {
	if h.pool == nil {
		return WhatIfResult{}, fmt.Errorf("HypoPG requires a database pool")
	}
	if err := ctx.Err(); err != nil {
		return WhatIfResult{}, err
	}
	queries = whatIfSample(explainableQueries(queries), nil)
	if len(queries) == 0 {
		return WhatIfResult{}, nil
	}
	return h.inSession(ctx, func(s *hypopgSession) (WhatIfResult, error) {
		if !s.canHide {
			return WhatIfResult{OldIndexPresent: true}, nil
		}
		return s.evaluateDrop(ctx, ix, queries)
	})
}
