package causal

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Disk / WAL runway: what fills the disk, or a slot's configured WAL
// maximum, over hours. A slow fill is invisible between two samples a
// few seconds apart, so the runway monitor's sampled trends decide slot
// and database growth; the archiver and a write surge are read as in the
// WAL retention family. Disk capacity is only the operator's declared
// capacity, never inferred.

// Disk/WAL runway thresholds.
const (
	slotTrendWeight   = 0.2
	slowTrendWeight   = 0.5
	growthWeight      = 0.5
	growthShareWeight = 0.2
	backlogWeight     = 0.1
	// slotMinR2 and growthMinR2 are the fits a growth trend needs.
	slotMinR2   = 0.5
	growthMinR2 = 0.6
	// slotGrowthMin is the least growth of a slot trend that counts.
	slotGrowthMin = 1 << 20
	// Database growth must add at least growthMinBytes and growthMinShare
	// of the databases' size over the sampled span.
	growthMinBytes = 1 << 20
	growthMinShare = 0.01
	// diskShare: database growth this share of the disk usage growth fills
	// the disk.
	diskShare = 0.5
)

// diskAmplifies holds only in this family. A write surge is the WAL that
// writes produce between the two close samples; checkpoints recycle it
// unless a slot or the archiver keeps it (their own hypotheses), so it
// cannot be what keeps filling the disk while the database files
// themselves grow steadily and materially: the surge then contributes to
// that growth, which is the root. Heavy concurrent WAL also swells pg_wal
// and so the disk usage growth, which can cost growth its share bonus;
// the share stays a confidence signal, never the tie-break. Without
// supported growth a surge is still a root.
var diskAmplifies = map[NodeID][]NodeID{WriteSurge: {DatabaseGrowth}}

// DiagnoseDiskWAL scores the disk/WAL runway hypotheses.
func DiagnoseDiskWAL(obs []Observation) Diagnosis {
	slots, m1 := walSeries(obs, probes.ReplicationSlots)
	wal, m2 := walSeries(obs, probes.WALCheckpoint)
	arch, m3 := walSeries(obs, probes.Archiver)
	changed, mID := walComparable(&slots, &wal, &arch)
	rate, m4 := measureRate(wal, changed)
	m4 = append(mID, m4...)
	trends, trendEv, m5 := readTrends(obs)
	dir, dirEv, m6 := readWALDirectory(obs)
	hs := []Hypothesis{scoreInactiveSlotRunway(slots, trends, trendEv),
		scoreSlowConsumerRunway(slots, rate, trends, trendEv),
		withBacklog(scoreArchiver(arch), dir, dirEv), scoreWriteSurge(rate),
		scoreDatabaseGrowth(trends, trendEv)}
	d := rankWith(FamilyDiskWAL, hs, diskAmplifies)
	for _, m := range [][]Missing{m1, m2, m3, m4, m5, m6} {
		d.Missing = append(d.Missing, m...)
	}
	d.Observed = runwayFacts(trends, trendEv)
	if rate.known {
		d.Observed = append(d.Observed, Fact{EvidenceID: rate.ev, Text: rateText(rate)})
	}
	if disk, ok := probes.FindTrend(trends, probes.RunwayDiskUsed,
		probes.SubjectCluster); !ok || !probes.Known(disk.Limit) {
		d.Missing = append(d.Missing, Missing{ProbeID: "disk_capacity",
			Reason: "not_declared"})
	}
	return d
}

func readTrends(obs []Observation) ([]probes.RunwayTrend, string, []Missing) {
	o, ok := find(obs, probes.RunwayTrendsProbe)
	if !ok || !o.Result.Status.Usable() {
		return nil, "", missingFor(obs, probes.RunwayTrendsProbe)
	}
	ts, err := probes.RunwayTrends(o.Result)
	if err != nil {
		return nil, "", []Missing{{ProbeID: probes.RunwayTrendsProbe, Status: o.Result.Status,
			Reason: "undecodable"}}
	}
	return ts, o.EvidenceID, nil
}

func readWALDirectory(obs []Observation) (probes.WALDirectory, string, []Missing) {
	o, ok := find(obs, probes.WALDirectoryProbe)
	if !ok || !o.Result.Status.Usable() {
		return probes.WALDirectory{}, "", missingFor(obs, probes.WALDirectoryProbe)
	}
	d, err := probes.WALDirectoryOf(o.Result)
	if err != nil {
		return probes.WALDirectory{}, "", []Missing{{ProbeID: probes.WALDirectoryProbe,
			Status: o.Result.Status, Reason: unavailableReason(o, err)}}
	}
	return d, o.EvidenceID, nil
}

// steadySlotGrowth reports a slot trend read from enough samples that
// rises steadily and materially (at least slotGrowthMin bytes over its
// span): a consumer that keeps up still drifts by kilobytes.
func steadySlotGrowth(tr probes.RunwayTrend) bool {
	return tr.Samples >= minTrendSamples && tr.RatePerS > 0 && tr.R2 >= slotMinR2 &&
		tr.RatePerS*tr.SpanS() >= slotGrowthMin
}

func trendText(tr probes.RunwayTrend) string {
	return fmt.Sprintf("%s bytes/s over %s s (%d samples)",
		probes.FormatValue(tr.RatePerS), probes.FormatValue(tr.SpanS()), tr.Samples)
}

// scoreInactiveSlotRunway is the WAL family's inactive slot, plus the
// slot's sampled growth.
func scoreInactiveSlotRunway(w walEvidence, ts []probes.RunwayTrend,
	ev string) Hypothesis {
	h := scoreInactiveSlot(w)
	all, ok := slotsOf(w.last)
	s, found := largest(all, false)
	if !ok || !found || len(h.Contradict) > 0 {
		return h
	}
	if tr, ok := probes.FindTrend(ts, probes.RunwayWALSlot, s.Name); ok &&
		steadySlotGrowth(tr) {
		h.add(slotTrendWeight, ev, "its retained WAL grew at "+trendText(tr))
	}
	return h
}

// scoreSlowConsumerRunway reads an active slot's sampled growth; without
// a trend, the two close samples decide as in the WAL family.
func scoreSlowConsumerRunway(w walEvidence, r walRate, ts []probes.RunwayTrend,
	ev string) Hypothesis {
	h := newHypothesis(SlowConsumer, "replication slots")
	all, ok := slotsOf(w.last)
	if !ok {
		return h
	}
	if _, found := largest(all, true); !found {
		noSlotContradiction(&h, w.last.EvidenceID, all, true)
		return h
	}
	s, tr, found := fastestActiveTrend(all, ts)
	if !found {
		return scoreSlowConsumer(w, r)
	}
	h.Subject = fmt.Sprintf("slot %q", s.Name)
	switch {
	case steadySlotGrowth(tr):
		h.add(slowTrendWeight, ev, fmt.Sprintf("slot %q is active but its retained WAL "+
			"grew at %s", s.Name, trendText(tr)))
		if s.RetainedBytes >= slotRetainedMin {
			h.add(0.2, w.last.EvidenceID, "it retains "+bytesText(s.RetainedBytes)+" of WAL")
		}
	case tr.RatePerS <= 0:
		h.contradict(ev, fmt.Sprintf("active slot %q keeps up: its retained WAL did not "+
			"grow over %s s (%d samples)", s.Name, probes.FormatValue(tr.SpanS()),
			tr.Samples))
	}
	return h
}

// fastestActiveTrend is the active slot whose sampled retention grows
// fastest, among those with a trend of enough samples.
func fastestActiveTrend(all []probes.Slot, ts []probes.RunwayTrend) (probes.Slot,
	probes.RunwayTrend, bool) {
	var best probes.Slot
	var bestTr probes.RunwayTrend
	found := false
	for _, s := range all {
		tr, ok := probes.FindTrend(ts, probes.RunwayWALSlot, s.Name)
		if !s.Active || !ok || tr.Samples < minTrendSamples || !probes.Known(tr.RatePerS) {
			continue
		}
		if !found || tr.RatePerS > bestTr.RatePerS {
			best, bestTr, found = s, tr, true
		}
	}
	return best, bestTr, found
}

// withBacklog adds the segments waiting for the archiver to a supported
// or open archiver hypothesis.
func withBacklog(h Hypothesis, dir probes.WALDirectory, ev string) Hypothesis {
	if ev == "" || len(h.Contradict) > 0 || !(dir.ReadyFiles > 0) {
		return h
	}
	h.add(backlogWeight, ev, probes.FormatValue(dir.ReadyFiles)+
		" WAL segments wait for the archiver")
	return h
}

// scoreDatabaseGrowth reads the databases' sampled size: steady, material
// growth supports it; a flat or falling series refutes it.
func scoreDatabaseGrowth(ts []probes.RunwayTrend, ev string) Hypothesis {
	h := newHypothesis(DatabaseGrowth, "databases")
	tr, ok := probes.FindTrend(ts, probes.RunwayDatabaseBytes, probes.SubjectCluster)
	if !ok || tr.Samples < minTrendSamples || !probes.Known(tr.RatePerS) {
		return h
	}
	if tr.RatePerS <= 0 {
		h.contradict(ev, fmt.Sprintf("the databases did not grow over %s s (%d samples)",
			probes.FormatValue(tr.SpanS()), tr.Samples))
		return h
	}
	growth := tr.RatePerS * tr.SpanS()
	if !(tr.R2 >= growthMinR2) || growth < math.Max(growthMinBytes,
		growthMinShare*tr.LastValue) {
		return h
	}
	h.add(growthWeight, ev, fmt.Sprintf("the databases grew steadily at %s, r2 %s",
		trendText(tr), probes.FormatValue(tr.R2)))
	disk, ok := probes.FindTrend(ts, probes.RunwayDiskUsed, probes.SubjectCluster)
	if ok && disk.RatePerS > 0 && tr.RatePerS >= diskShare*disk.RatePerS {
		h.add(growthShareWeight, ev, fmt.Sprintf("that is %s%% of the disk usage growth "+
			"of %s bytes/s", probes.FormatValue(math.Round(100*tr.RatePerS/disk.RatePerS)),
			probes.FormatValue(disk.RatePerS)))
	}
	return h
}
