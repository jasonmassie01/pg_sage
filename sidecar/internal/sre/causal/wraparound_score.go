package causal

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// scoreHolder scores one kind of xmin-horizon holder: the oldest holder
// of the node's kinds pins the table when its xmin age is at least half
// the table's XID age, and cannot when it is under a tenth of it.
func scoreHolder(w wrapEvidence, id NodeID) Hypothesis {
	h := newHypothesis(id, "xmin horizon")
	if !w.holdersOK {
		return h
	}
	holder, found := oldestOf(w.holders, holderKinds[id])
	if !found {
		h.contradict(w.holdersEv, noHolderText(id, len(w.holders) == 0))
		return h
	}
	h.Subject = holderSubject(holder)
	age, tableAge := holder.XminAge, w.table.XIDAge
	if !probes.Known(age) || !(tableAge > 0) {
		return h
	}
	switch {
	case age >= pinShare*tableAge:
		h.add(mechWeight, w.holdersEv, fmt.Sprintf("%s holds the xmin horizon at age %s; "+
			"table %s is at XID age %s", describeHolder(holder), probes.FormatValue(age),
			w.table.Relation, probes.FormatValue(tableAge)))
		if age >= w.holders[0].XminAge {
			h.add(oldestWeight, w.holdersEv, "it is the oldest holder of the horizon")
		}
		w.addNear(&h)
	case age < refuteShare*tableAge:
		h.contradict(w.holdersEv, fmt.Sprintf("%s holds the horizon at age %s, under a "+
			"tenth of table %s's XID age %s: vacuum can freeze past it",
			describeHolder(holder), probes.FormatValue(age), w.table.Relation,
			probes.FormatValue(tableAge)))
	}
	return h
}

// oldestOf is the first (oldest) holder of the given kinds.
func oldestOf(hs []probes.XminHolder, kinds []string) (probes.XminHolder, bool) {
	for _, h := range hs {
		for _, k := range kinds {
			if h.Kind == k {
				return h, true
			}
		}
	}
	return probes.XminHolder{}, false
}

func noHolderText(id NodeID, none bool) string {
	if none {
		return "nothing holds the xmin horizon of this database"
	}
	switch id {
	case XminHeldBySession:
		return "no session holds the horizon"
	case XminHeldByPreparedXact:
		return "no prepared transaction holds the horizon"
	default:
		return "no replication slot or standby holds the horizon"
	}
}

func holderSubject(h probes.XminHolder) string {
	switch h.Kind {
	case probes.HolderSession:
		return fmt.Sprintf("pid %d", h.PID)
	case probes.HolderStandby:
		return fmt.Sprintf("standby pid %d", h.PID)
	case probes.HolderPreparedXact:
		return "prepared transaction " + h.Name
	default:
		return fmt.Sprintf("slot %q", h.Name)
	}
}

func describeHolder(h probes.XminHolder) string {
	switch h.Kind {
	case probes.HolderSession:
		return fmt.Sprintf("pid %d (%s)", h.PID, h.State)
	case probes.HolderStandby:
		return fmt.Sprintf("standby feedback through pid %d", h.PID)
	case probes.HolderPreparedXact:
		return "prepared transaction " + h.Name
	case probes.HolderSlotCatalog:
		return fmt.Sprintf("slot %q (%s, catalog_xmin)", h.Name, h.State)
	default:
		return fmt.Sprintf("slot %q (%s)", h.Name, h.State)
	}
}

// scoreSaturated: every worker busy in every sample.
func scoreSaturated(w wrapEvidence) Hypothesis {
	h := newHypothesis(AutovacuumSaturated, "autovacuum")
	last, err := probes.XIDRunwayOf(w.xid.last.Result)
	if w.xid.n == 0 || err != nil {
		return h
	}
	ev := w.xid.last.EvidenceID
	if !last.AutovacuumOn {
		h.contradict(ev, "autovacuum is off")
		return h
	}
	if !last.WorkersSaturated() {
		h.contradict(ev, fmt.Sprintf("only %s of %s autovacuum workers were busy",
			probes.FormatValue(last.Workers), probes.FormatValue(last.MaxWorkers)))
		return h
	}
	when := "in the sample"
	if w.xid.n > 1 {
		first, err := probes.XIDRunwayOf(w.xid.first.Result)
		if err != nil || !first.WorkersSaturated() {
			return h
		}
		when = "in both samples"
	}
	h.add(mechWeight, ev, fmt.Sprintf("all %s of %s autovacuum workers were busy %s",
		probes.FormatValue(last.Workers), probes.FormatValue(last.MaxWorkers), when))
	w.addNear(&h)
	return h
}

// scoreDisabled: autovacuum off globally, or for the table.
func scoreDisabled(w wrapEvidence) Hypothesis {
	h := newHypothesis(AutovacuumDisabled, "table "+w.table.Relation)
	last, err := probes.XIDRunwayOf(w.xid.last.Result)
	known := w.xid.n > 0 && err == nil
	switch {
	case known && !last.AutovacuumOn:
		h.Subject = "autovacuum"
		h.add(mechWeight, w.xid.last.EvidenceID, "autovacuum is off")
	case !w.table.AutovacuumEnabled:
		h.add(mechWeight, w.tableEv, fmt.Sprintf("table %s has autovacuum_enabled = false",
			w.table.Relation))
	case known:
		h.contradict(w.tableEv, fmt.Sprintf("autovacuum is on and table %s has it enabled",
			w.table.Relation))
		return h
	default:
		return h
	}
	w.addNear(&h)
	return h
}

// scoreCancelled: logged cancellations support it; none logged is never
// proof, since the log source may be missing.
func scoreCancelled(w wrapEvidence) Hypothesis {
	h := newHypothesis(AutovacuumCancelled, "autovacuum")
	if !w.cancelOK || w.cancels <= 0 {
		return h
	}
	h.add(mechWeight, w.cancelEv, fmt.Sprintf("%d incidents of cancelled autovacuum tasks "+
		"were logged in the window", w.cancels))
	w.addNear(&h)
	return h
}

// scoreXIDSurge compares the XID rate between the two samples with the
// measured trend.
func scoreXIDSurge(w wrapEvidence) Hypothesis {
	h := newHypothesis(XIDConsumptionSurge, "transaction IDs")
	short, ok := xidSampleRate(w.xid)
	long := w.trend.RatePerS
	if !ok || !w.trendOK || !probes.Known(long) || long < 0 {
		return h
	}
	ev := w.xid.last.EvidenceID
	text := fmt.Sprintf("XIDs were consumed at %s per second between the samples; the "+
		"trend is %s per second", probes.FormatValue(short), probes.FormatValue(long))
	switch {
	case short >= xidSurgeFloor && short >= xidSurgeRatio*long:
		h.add(mechWeight, ev, text)
		w.addNear(&h)
	case short <= xidCalmRatio*long:
		h.contradict(ev, text+", within 1.5 times it")
	}
	return h
}

// xidSampleRate is the XID consumption per second between the samples.
func xidSampleRate(s walEvidence) (float64, bool) {
	if s.n < 2 {
		return 0, false
	}
	a, errA := probes.XIDRunwayOf(s.first.Result)
	b, errB := probes.XIDRunwayOf(s.last.Result)
	span := s.last.Result.ObservedAt.Sub(s.first.Result.ObservedAt).Seconds()
	if errA != nil || errB != nil || span <= 0 || !probes.Known(a.NextXID) ||
		!probes.Known(b.NextXID) || b.NextXID < a.NextXID {
		return 0, false
	}
	return (b.NextXID - a.NextXID) / span, true
}
