package causal

import (
	"fmt"
	"math"
	"sort"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// WAL retention family: an inactive slot, a slow consumer, archiver
// failure and a write surge, from up to two samples of the slots, WAL
// volume and archiver. A surge only amplifies slot retention; disk
// capacity is never inferred from database sizes and stays unknown.

// WAL thresholds.
const (
	slotRetainedMin      = 16 << 20
	slowConsumerMinBytes = 1 << 20
	surgeMinRate         = 4 << 20 // bytes per second
	surgeRatio           = 3
	strongSurgeRatio     = 10
	calmRatio            = 1.5
)

// walEvidence is the first and last usable observation of one probe.
type walEvidence struct {
	first, last Observation
	n           int
}

// walRate is the WAL rate between the WAL samples, when comparable.
type walRate struct {
	known       bool
	rate, avg   float64 // bytes per second; avg NaN when unknown
	delta, span float64
	ev          string
}

// DiagnoseWAL scores the WAL retention hypotheses.
func DiagnoseWAL(obs []Observation) Diagnosis {
	slots, m1 := walSeries(obs, probes.ReplicationSlots)
	wal, m2 := walSeries(obs, probes.WALCheckpoint)
	arch, m3 := walSeries(obs, probes.Archiver)
	rate, m4 := measureRate(wal)
	missing := append(append(append(m1, m2...), m3...), m4...)
	hs := []Hypothesis{scoreInactiveSlot(slots), scoreSlowConsumer(slots, rate),
		scoreArchiver(arch), scoreWriteSurge(rate)}
	d := rank(FamilyWAL, hs)
	d.Missing = append(missing, Missing{ProbeID: "disk_capacity",
		Reason: "provider_metric_unavailable"})
	if rate.known {
		d.Observed = []Fact{{EvidenceID: rate.ev, Text: rateText(rate)}}
	}
	return d
}

// walSeries returns the usable observations of one probe in time order.
func walSeries(obs []Observation, id probes.ID) (walEvidence, []Missing) {
	var usable []Observation
	var missing []Missing
	for _, o := range obs {
		if o.Result.ProbeID != id {
			continue
		}
		if !o.Result.Status.Usable() {
			missing = append(missing, Missing{ProbeID: id, Status: o.Result.Status,
				Reason: o.Result.Reason})
			continue
		}
		usable = append(usable, o)
	}
	if len(usable) == 0 {
		if len(missing) == 0 {
			missing = missingFor(obs, id)
		}
		return walEvidence{}, missing
	}
	sort.SliceStable(usable, func(i, j int) bool {
		return usable[i].Result.ObservedAt.Before(usable[j].Result.ObservedAt)
	})
	return walEvidence{first: usable[0], last: usable[len(usable)-1],
		n: len(usable)}, missing
}

// measureRate compares the WAL volume samples. A statistics reset or a
// falling counter makes the rate unknown (CHECK-07), never negative.
func measureRate(w walEvidence) (walRate, []Missing) {
	r := walRate{ev: w.last.EvidenceID, avg: math.NaN()}
	if w.n == 0 {
		return r, nil
	}
	a, errA := probes.WALStats(w.first.Result)
	b, errB := probes.WALStats(w.last.Result)
	if errA != nil || errB != nil || !probes.Known(b.WALBytes) {
		return r, []Missing{{ProbeID: probes.WALCheckpoint, Reason: "wal_bytes_unknown"}}
	}
	if span := w.last.Result.ObservedAt.Sub(b.StatsReset).Seconds(); !b.StatsReset.IsZero() &&
		span > 0 {
		r.avg = b.WALBytes / span
	}
	if w.n < 2 {
		return r, []Missing{{ProbeID: probes.WALCheckpoint,
			Reason: "second_sample_unavailable"}}
	}
	r.delta = b.WALBytes - a.WALBytes
	r.span = w.last.Result.ObservedAt.Sub(w.first.Result.ObservedAt).Seconds()
	if !a.StatsReset.Equal(b.StatsReset) || r.delta < 0 || !probes.Known(a.WALBytes) {
		return r, []Missing{{ProbeID: probes.WALCheckpoint, Reason: "counter_reset"}}
	}
	if r.span <= 0 {
		return r, []Missing{{ProbeID: probes.WALCheckpoint, Reason: "samples_out_of_order"}}
	}
	r.known, r.rate = true, r.delta/r.span
	return r, nil
}

func rateText(r walRate) string {
	return fmt.Sprintf("WAL was written at %s bytes/s over %s s",
		probes.FormatValue(math.Round(r.rate)), probes.FormatValue(r.span))
}

func bytesText(b float64) string { return probes.FormatValue(b) + " bytes" }

func slotsOf(o Observation) ([]probes.Slot, bool) {
	if o.Result.ProbeID != probes.ReplicationSlots {
		return nil, false
	}
	s, err := probes.Slots(o.Result)
	return s, err == nil
}

func slotByName(ss []probes.Slot, name string) (probes.Slot, bool) {
	for _, s := range ss {
		if s.Name == name {
			return s, true
		}
	}
	return probes.Slot{}, false
}

// largest returns the slot with the most retained WAL among those that
// match active (unknown retention sorts last).
func largest(ss []probes.Slot, active bool) (probes.Slot, bool) {
	var best probes.Slot
	found := false
	for _, s := range ss {
		if s.Active != active {
			continue
		}
		if !found || retained(s) > retained(best) {
			best, found = s, true
		}
	}
	return best, found
}

func retained(s probes.Slot) float64 {
	if !probes.Known(s.RetainedBytes) {
		return -1
	}
	return s.RetainedBytes
}

func noSlotContradiction(h *Hypothesis, ev string, all []probes.Slot, active bool) {
	switch {
	case len(all) == 0:
		h.contradict(ev, "no replication slots exist")
	case active:
		h.contradict(ev, "no replication slot is active")
	default:
		h.contradict(ev, "every replication slot is active")
	}
}
