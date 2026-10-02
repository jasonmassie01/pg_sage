package causal

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Pool exhaustion at an external pooler (CHECK-04, Sage SRE follow-ups
// B): with PgBouncer telemetry, clients queueing at the pooler for a
// server connection are told apart from backend exhaustion (the
// saturation fact) and a lock backlog, instead of reading as "no
// connection pressure" while PostgreSQL itself has headroom.

// PoolerSaturation is pool exhaustion at the pooler.
const PoolerSaturation NodeID = "pooler_saturation"

// ReasonPoolerUnavailable says no pooler telemetry was collected: no
// pooler is configured for the database (sre.poolers).
const ReasonPoolerUnavailable = "pooler_telemetry_unavailable"

// poolerMaxWaitS is the client wait at which queueing is an incident
// rather than a burst (PgBouncer runbooks alert on maxwait over 1 s).
const poolerMaxWaitS = 1.0

var poolerNodes = []Node{
	{ID: PoolerSaturation, Family: FamilyConnections, Label: "pool exhaustion at the pooler",
		Mechanism: "Every server connection of an external pooler's pool is busy, so " +
			"clients queue at the pooler before they reach PostgreSQL.",
		Predicted: "cl_waiting above 0 at a PgBouncer pool, the oldest client waiting a " +
			"second or more, no idle server connections",
		Refutation: string(probes.PoolerPools),
		Confounders: "slow queries or lock waits holding the pool's server connections; " +
			"a pool sized below the application's concurrency",
		Amplifies: []NodeID{BlockedBacklog},
		OperatorStep: "Find what holds the pool's server connections (slow queries, lock " +
			"waits, long transactions) and fix that first. Resize the pool only within " +
			"the database's connection headroom, through your normal change process."},
}

// poolerEvidence is the usable pooler telemetry: the first and last
// samples, and the poolers that could not be read.
type poolerEvidence struct {
	first, last Observation
	n           int
	missing     []Missing
}

func poolerSamples(obs []Observation) poolerEvidence {
	var e poolerEvidence
	series, missing := usableSeries(obs, probes.PoolerPools)
	for _, m := range missing {
		if m.Reason == "not_collected" {
			m.Reason = ReasonPoolerUnavailable
		}
		e.missing = append(e.missing, m)
	}
	seen := map[string]bool{}
	for _, o := range series {
		_, failures, err := probes.PoolerPoolsOf(o.Result)
		if err != nil {
			e.missing = append(e.missing, Missing{ProbeID: probes.PoolerPools,
				Status: o.Result.Status, Reason: err.Error()})
			continue
		}
		for _, f := range failures {
			if !seen[f.Pooler+"/"+f.Reason] {
				seen[f.Pooler+"/"+f.Reason] = true
				e.missing = append(e.missing, Missing{ProbeID: probes.PoolerPools,
					Status: probes.StatusError, Reason: f.Reason})
			}
		}
		if e.n == 0 {
			e.first = o
		}
		e.last, e.n = o, e.n+1
	}
	return e
}

// busiestPool is the pool with the most waiting clients (then the
// longest wait, then the name); the admin console is never a pool.
func busiestPool(o Observation) (probes.PoolerPool, bool) {
	pools, _, err := probes.PoolerPoolsOf(o.Result)
	if err != nil {
		return probes.PoolerPool{}, false
	}
	var best probes.PoolerPool
	found := false
	for _, p := range pools {
		if p.AdminConsole() {
			continue
		}
		if !found || morePressed(p, best) {
			best, found = p, true
		}
	}
	return best, found
}

func morePressed(a, b probes.PoolerPool) bool {
	wa, wb := orZero(a.ClientWaiting), orZero(b.ClientWaiting)
	if wa != wb {
		return wa > wb
	}
	if ma, mb := orZero(a.MaxWaitS), orZero(b.MaxWaitS); ma != mb {
		return ma > mb
	}
	return poolKey(a) < poolKey(b)
}

func orZero(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return v
}

func poolKey(p probes.PoolerPool) string { return p.Pooler + "/" + p.Database + "/" + p.User }

func poolSubject(p probes.PoolerPool) string {
	return fmt.Sprintf("pool %q at pooler %q", p.Database, p.Pooler)
}

// samePool finds p's pool in another sample.
func samePool(o Observation, p probes.PoolerPool) (probes.PoolerPool, bool) {
	pools, _, err := probes.PoolerPoolsOf(o.Result)
	if err != nil {
		return probes.PoolerPool{}, false
	}
	for _, q := range pools {
		if poolKey(q) == poolKey(p) {
			return q, true
		}
	}
	return probes.PoolerPool{}, false
}

// scorePooler scores pool exhaustion at the pooler from its busiest pool.
// Without an observed pool there is no hypothesis: the telemetry is
// missing, not healthy.
func scorePooler(e poolerEvidence) (Hypothesis, bool) {
	if e.n == 0 {
		return Hypothesis{}, false
	}
	p, ok := busiestPool(e.last)
	if !ok {
		return Hypothesis{}, false
	}
	h := newHypothesis(PoolerSaturation, poolSubject(p))
	ev := e.last.EvidenceID
	if !probes.Known(p.ClientWaiting) {
		return h, true
	}
	if p.ClientWaiting <= 0 {
		h.contradict(ev, fmt.Sprintf("no client waits at pooler %q", p.Pooler))
		return h, true
	}
	h.add(0.3, ev, fmt.Sprintf("%s clients wait at pool %q of pooler %q",
		fv(p.ClientWaiting), p.Database, p.Pooler))
	if probes.Known(p.MaxWaitS) && p.MaxWaitS >= poolerMaxWaitS {
		h.add(0.2, ev, fmt.Sprintf("the oldest has waited %s s", fv(p.MaxWaitS)))
	}
	if q, ok := samePool(e.first, p); ok && e.n > 1 && orZero(q.ClientWaiting) > 0 {
		h.add(0.2, e.first.EvidenceID, fmt.Sprintf("%s clients were already waiting %s s "+
			"earlier", fv(q.ClientWaiting), fv(spanSeconds(e.first, e.last))))
	}
	if probes.Known(p.ServerIdle) && p.ServerIdle == 0 && orZero(p.ServerActive) > 0 {
		h.add(0.1, ev, fmt.Sprintf("0 idle server connections: all %s are busy",
			fv(p.ServerActive)))
	}
	return h, true
}

// poolerFacts states the busiest pool's state, whatever it is.
func poolerFacts(e poolerEvidence) []Fact {
	if e.n == 0 {
		return nil
	}
	p, ok := busiestPool(e.last)
	if !ok {
		return nil
	}
	text := fmt.Sprintf("pooler %q pool %q: %s clients waiting", p.Pooler, p.Database,
		fv(orZero(p.ClientWaiting)))
	if probes.Known(p.MaxWaitS) {
		text += fmt.Sprintf(" (oldest %s s)", fv(p.MaxWaitS))
	}
	if probes.Known(p.ServerActive) && probes.Known(p.ServerIdle) {
		text += fmt.Sprintf(", %s of %s server connections busy", fv(p.ServerActive),
			fv(p.ServerActive+p.ServerIdle))
	}
	return []Fact{{EvidenceID: e.last.EvidenceID, Text: text}}
}
