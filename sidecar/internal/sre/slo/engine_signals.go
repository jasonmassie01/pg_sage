package slo

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sampleProxies takes one slice from every proxy and appends it to the
// proxy's cumulative counters (resumed from the newest stored sample).
// A sample is stored every tick, so freshness and coverage stay honest
// even when the proxy could not measure.
func (e *Engine) sampleProxies(ctx context.Context, scope sre.Scope, now time.Time) {
	series := string(scope.DatabaseID)
	for name, p := range e.proxies {
		total, ok := e.proxyTotal(ctx, scope, name, series)
		if !ok {
			continue
		}
		h := storeHistory{store: e.store, dep: scope.DeploymentID, slo: name, series: series}
		s := p.Slice(ctx, now, h)
		bad := math.Min(math.Max(s.Bad, 0), math.Max(s.Eligible, 0))
		total.Bad += bad
		total.Eligible += math.Max(s.Eligible, 0)
		total.ObservedAt = now
		if _, err := e.store.RecordSample(ctx, scope.DeploymentID, name, total,
			s.Value); err != nil {
			e.logf("WARN", "sre slo: recording proxy %s failed: %v", name, err)
			continue
		}
		e.mu.Lock()
		e.totals[name] = total
		e.reasons[name] = s.Reason
		e.mu.Unlock()
	}
}

// proxyTotal is the proxy's cumulative counter, read from the store once.
func (e *Engine) proxyTotal(ctx context.Context, scope sre.Scope, name,
	series string) (PushSample, bool) {
	e.mu.Lock()
	total, ok := e.totals[name]
	e.mu.Unlock()
	if ok {
		return total, true
	}
	last, found, err := e.store.LastSample(ctx, scope.DeploymentID, name, series)
	if err != nil {
		e.logf("WARN", "sre slo: resuming proxy %s failed: %v", name, err)
		return PushSample{}, false
	}
	if !found {
		last = PushSample{Series: series}
	}
	return last, true
}

// storeHistory is a proxy's own stored gauge history.
type storeHistory struct {
	store  *Store
	dep    sre.UUID
	slo    string
	series string
}

func (h storeHistory) Baseline(ctx context.Context, since time.Time) (float64, int, error) {
	return h.store.Baseline(ctx, h.dep, h.slo, h.series, since)
}

// Probe is the slo_status signal probe: every SLO's error-budget state
// as typed evidence. Unknown numbers are null; an unreadable store is an
// error result, never an empty (healthy) one.
func (e *Engine) Probe(ctx context.Context, _ probes.Args) probes.Result {
	res := probes.Result{ProbeID: probes.SLOStatus, Version: "v1", ObservedAt: time.Now()}
	start := time.Now()
	sts, err := e.Statuses(ctx)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Status, res.Reason = probes.StatusError, "store_unavailable"
		res.Error = truncateText(err.Error(), 200)
		return res
	}
	res.Columns = []string{"name", "kind", "state", "fast_burning", "customer_impact",
		"burn_long", "burn_short", "long_window", "short_window", "unknown",
		"budget_remaining", "evaluated_at", "age_s"}
	for _, st := range sts {
		res.Rows = append(res.Rows, statusRow(st, res.ObservedAt))
	}
	res.Status = probes.StatusOK
	if len(res.Rows) == 0 {
		res.Status, res.Reason = probes.StatusEmpty, "no_slos"
	}
	return res
}

func statusRow(st Status, at time.Time) probes.Row {
	row := probes.Row{"name": st.Name, "kind": string(st.Kind), "state": string(st.State),
		"fast_burning": st.FastBurning, "customer_impact": st.CustomerImpact,
		"burn_long": nil, "burn_short": nil, "long_window": "", "short_window": "",
		"unknown": strings.Join(st.Unknown, ","), "budget_remaining": nil,
		"evaluated_at": st.EvaluatedAt, "age_s": at.Sub(st.EvaluatedAt).Seconds()}
	if r, ok := headlineRule(st); ok {
		row["long_window"], row["short_window"] = r.Long.Window, r.Short.Window
		if r.Long.BurnRate != nil {
			row["burn_long"] = *r.Long.BurnRate
		}
		if r.Short.BurnRate != nil {
			row["burn_short"] = *r.Short.BurnRate
		}
	}
	if st.BudgetRemaining != nil {
		row["budget_remaining"] = *st.BudgetRemaining
	}
	return row
}

// headlineRule is the rule a status is summarized by: the first firing
// rule, else the first rule.
func headlineRule(st Status) (RuleResult, bool) {
	for _, r := range st.Rules {
		if r.Firing {
			return r, true
		}
	}
	if len(st.Rules) == 0 {
		return RuleResult{}, false
	}
	return st.Rules[0], true
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
