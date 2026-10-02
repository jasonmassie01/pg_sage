package causal

import (
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Connection pressure family: pool fan-out, a blocked-query backlog and
// a connection leak, from up to two connection_saturation samples (the
// first and the last) and the lock graph. Applications are attributed
// only from this database's sessions; saturation is cluster-wide,
// because max_connections is.

// Connection thresholds.
const (
	fanOutMinIdle     = 10
	leakMinGrowth     = 3
	backlogMinWaiting = 3
)

// connSample is one usable connection_saturation observation.
type connSample struct {
	ev      string
	obs     Observation
	groups  []probes.ConnGroup
	idle    map[string]int64 // this database's idle backends per application
	total   map[string]int64 // this database's backends per application
	active  int64
	waiting int64
}

func newConnSample(o Observation, gs []probes.ConnGroup) connSample {
	s := connSample{ev: o.EvidenceID, obs: o, groups: gs, idle: map[string]int64{},
		total: map[string]int64{}}
	for _, g := range gs {
		if !g.InCurrentDatabase {
			continue
		}
		s.total[g.Application] += g.Backends
		s.waiting += g.WaitingOnLock
		switch g.State {
		case "idle":
			s.idle[g.Application] += g.Backends
		case "active":
			s.active += g.Backends
		}
	}
	return s
}

// connComparison is whether the first and last samples may be compared.
type connComparison struct {
	first, last connSample
	valid       bool
}

// DiagnoseConnections scores the connection pressure hypotheses, with
// pool exhaustion at an external pooler when its telemetry was collected
// (and the telemetry stated missing when it was not).
func DiagnoseConnections(obs []Observation) Diagnosis {
	samples, missing := connSamples(obs)
	pooler := poolerSamples(obs)
	if len(samples) == 0 {
		return Diagnosis{Family: FamilyConnections, GraphVersion: GraphVersion,
			Missing: append(missing, pooler.missing...),
			Reason:  "connection evidence unavailable"}
	}
	cmp, reason := compareConn(samples)
	if reason != "" {
		missing = append(missing, Missing{ProbeID: probes.ConnectionSaturation,
			Reason: reason})
	}
	last := cmp.last
	fanApp := busiestIdle(last)
	leakApp := growthApp(cmp, fanApp)
	hs := []Hypothesis{scoreFanOut(cmp, fanApp), scoreBacklog(last, obs),
		scoreLeak(cmp, leakApp)}
	if h, ok := scorePooler(pooler); ok {
		hs = append(hs, h)
	}
	d := rankWith(FamilyConnections, hs, leakBaseline(fanApp, leakApp))
	d.Missing = append(missing, pooler.missing...)
	d.Observed = append(saturation(last), poolerFacts(pooler)...)
	return d
}

// leakBaseline makes another application's pool contribute to a leak:
// a pool cannot explain pressure that grows, so beside a supported leak
// it is the baseline the leak grows on, not a competing root. A pool of
// the leaking application itself is that leak, not a separate baseline.
func leakBaseline(fanApp, leakApp string) map[NodeID][]NodeID {
	if fanApp == leakApp {
		return nil
	}
	return map[NodeID][]NodeID{PoolFanOut: {ConnectionLeak}}
}

// connSamples returns the usable samples in time order and the missing
// evidence (not collected, unavailable or unreadable).
func connSamples(obs []Observation) ([]connSample, []Missing) {
	var out []connSample
	var missing []Missing
	for _, o := range obs {
		if o.Result.ProbeID != probes.ConnectionSaturation {
			continue
		}
		gs, err := probes.ConnectionGroups(o.Result)
		if err != nil {
			missing = append(missing, Missing{ProbeID: o.Result.ProbeID,
				Status: o.Result.Status, Reason: unavailableReason(o, err)})
			continue
		}
		out = append(out, newConnSample(o, gs))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].obs.Result.ObservedAt.Before(out[j].obs.Result.ObservedAt)
	})
	if len(out) == 0 && len(missing) == 0 {
		missing = missingFor(obs, probes.ConnectionSaturation)
	}
	return out, missing
}

func unavailableReason(o Observation, err error) string {
	if o.Result.Reason != "" {
		return o.Result.Reason
	}
	return err.Error()
}

// compareConn decides whether growth between the samples is meaningful:
// it needs two samples of the same server incarnation (CHECK-07: no
// restart, failover or other server between them), taken at different
// instants (two samples of one instant that differ contradict each
// other). A sample without its start time cannot be matched.
func compareConn(samples []connSample) (connComparison, string) {
	c := connComparison{first: samples[0], last: samples[len(samples)-1]}
	if len(samples) < 2 {
		return c, "second_sample_unavailable"
	}
	if !c.last.obs.Result.ObservedAt.After(c.first.obs.Result.ObservedAt) {
		return c, "samples_out_of_order"
	}
	a, b := sampleIdentity(c.first), sampleIdentity(c.last)
	if reason := identityChange(a, b); reason != "" {
		return c, reason
	}
	if a.StartedAt.IsZero() || b.StartedAt.IsZero() {
		return c, ReasonServerRestarted
	}
	c.valid = true
	return c, ""
}

// sampleIdentity is the server incarnation that produced the sample.
func sampleIdentity(s connSample) probes.ServerIdentity {
	for _, g := range s.groups {
		if !g.Identity.IsZero() {
			return g.Identity
		}
	}
	return probes.ServerIdentity{}
}

// busiestIdle is this database's application with the most idle
// backends (ties to the lower name, so the choice is deterministic).
func busiestIdle(s connSample) string {
	best, bestN := "", int64(-1)
	for app, n := range s.idle {
		if n > bestN || (n == bestN && app < best) {
			best, bestN = app, n
		}
	}
	return best
}

// growthApp is the application whose idle backends grew the most, or
// fallback when none grew or the samples cannot be compared.
func growthApp(c connComparison, fallback string) string {
	if !c.valid {
		return fallback
	}
	best, bestDelta := fallback, int64(0)
	for app, n := range c.last.idle {
		d := n - c.first.idle[app]
		if d > bestDelta || (d == bestDelta && d > 0 && app < best) {
			best, bestDelta = app, d
		}
	}
	return best
}

func appSubject(app string) string { return fmt.Sprintf("application %q", app) }

func spanText(c connComparison) string {
	d := c.last.obs.Result.ObservedAt.Sub(c.first.obs.Result.ObservedAt)
	return probes.FormatValue(d.Seconds()) + " s"
}

func scoreFanOut(c connComparison, app string) Hypothesis {
	last := c.last
	h := newHypothesis(PoolFanOut, appSubject(app))
	if last.waiting >= backlogMinWaiting {
		h.contradict(last.ev, fmt.Sprintf("%d backends wait on locks: a backlog, "+
			"not idle pools", last.waiting))
	}
	idle := last.idle[app]
	if idle < fanOutMinIdle {
		return h
	}
	h.add(0.5, last.ev, fmt.Sprintf("application %q holds %d idle backends", app, idle))
	if c.valid {
		before := c.first.idle[app]
		if d := idle - before; d < leakMinGrowth && d > -leakMinGrowth {
			h.add(0.15, last.ev, fmt.Sprintf("the pool is stable: %d to %d idle in %s",
				before, idle, spanText(c)))
		}
	}
	if total := last.total[app]; total > 0 && idle*5 >= total*4 {
		h.add(0.1, last.ev, fmt.Sprintf("%d of its %d backends are idle", idle, total))
	}
	return h
}

func scoreBacklog(last connSample, obs []Observation) Hypothesis {
	h := newHypothesis(BlockedBacklog, "lock waits")
	if last.waiting == 0 {
		h.contradict(last.ev, "no backend waits on a lock")
		return h
	}
	if last.waiting >= backlogMinWaiting {
		h.add(0.5, last.ev, fmt.Sprintf("%d backends wait on locks", last.waiting))
	}
	if last.active > 0 && last.waiting*2 >= last.active {
		h.add(0.2, last.ev, fmt.Sprintf("that is %d of %d active backends",
			last.waiting, last.active))
	}
	if g, ok := find(obs, probes.LockGraph); ok && g.Result.Status.Usable() {
		if edges, err := probes.LockEdges(g.Result); err == nil && len(edges) > 0 {
			if root, found := rootFromGraph(edges, g.EvidenceID); found {
				h.add(0.1, g.EvidenceID, fmt.Sprintf("the lock graph shows root "+
					"blocker pid %d", root.pid))
			}
		}
	}
	return h
}

func scoreLeak(c connComparison, app string) Hypothesis {
	h := newHypothesis(ConnectionLeak, appSubject(app))
	if !c.valid {
		return h
	}
	before, after := c.first.idle[app], c.last.idle[app]
	switch d := after - before; {
	case d <= 0:
		h.contradict(c.last.ev, fmt.Sprintf("idle backends of %q did not grow "+
			"(%d to %d in %s)", app, before, after, spanText(c)))
	case d >= leakMinGrowth:
		h.add(0.45, c.last.ev, fmt.Sprintf("idle backends of %q grew from %d to %d "+
			"in %s", app, before, after, spanText(c)))
		if d >= 2*leakMinGrowth {
			h.add(0.2, c.last.ev, fmt.Sprintf("that is %d more backends", d))
		}
		if c.last.waiting == 0 {
			h.add(0.1, c.last.ev, "no lock waits explain the growth")
		}
	}
	return h
}

// saturation states the cluster-wide use of usable connections.
func saturation(s connSample) []Fact {
	if len(s.groups) == 0 {
		return nil
	}
	g := s.groups[0]
	usable := g.MaxConnections - g.ReservedConnections
	if usable <= 0 {
		return nil
	}
	pct := float64(g.TotalClientBackends) * 100 / float64(usable)
	return []Fact{{EvidenceID: s.ev, Text: fmt.Sprintf("%d of %d usable connections "+
		"are in use (%s%%)", g.TotalClientBackends, usable, probes.FormatValue(pct))}}
}
