package causal

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func scoreInactiveSlot(w walEvidence) Hypothesis {
	h := newHypothesis(InactiveSlot, "replication slots")
	all, ok := slotsOf(w.last)
	if !ok {
		return h
	}
	ev := w.last.EvidenceID
	s, found := largest(all, false)
	if !found {
		noSlotContradiction(&h, ev, all, false)
		return h
	}
	h.Subject = fmt.Sprintf("slot %q", s.Name)
	h.add(0.4, ev, fmt.Sprintf("slot %q is inactive", s.Name))
	if s.RetainedBytes >= slotRetainedMin {
		h.add(0.2, ev, "it retains "+bytesText(s.RetainedBytes)+" of WAL")
	}
	if w.n > 1 {
		before, _ := slotsOf(w.first)
		if b, ok := slotByName(before, s.Name); ok && probes.Known(b.RetainedBytes) &&
			s.RetainedBytes > b.RetainedBytes {
			h.add(0.15, ev, fmt.Sprintf("its retained WAL grew from %s to %s",
				bytesText(b.RetainedBytes), bytesText(s.RetainedBytes)))
		}
	}
	if s.WALStatus == "unreserved" || s.WALStatus == "lost" {
		h.add(0.1, ev, fmt.Sprintf("its wal_status is %s", s.WALStatus))
	}
	return h
}

func scoreSlowConsumer(w walEvidence, r walRate) Hypothesis {
	h := newHypothesis(SlowConsumer, "replication slots")
	all, ok := slotsOf(w.last)
	if !ok {
		return h
	}
	ev := w.last.EvidenceID
	if _, found := largest(all, true); !found {
		noSlotContradiction(&h, ev, all, true)
		return h
	}
	if w.n < 2 {
		return h
	}
	before, _ := slotsOf(w.first)
	s, from, growth := fastestGrowingActive(all, before)
	h.Subject = fmt.Sprintf("slot %q", s.Name)
	if growth <= 0 {
		h.contradict(ev, fmt.Sprintf("active slot %q keeps up: its retained WAL went "+
			"from %s to %s", s.Name, bytesText(from), bytesText(s.RetainedBytes)))
		return h
	}
	threshold := float64(slowConsumerMinBytes)
	if r.known && r.delta/2 > threshold {
		threshold = r.delta / 2
	}
	if growth >= threshold {
		h.add(0.45, ev, fmt.Sprintf("slot %q is active but its retained WAL grew from "+
			"%s to %s", s.Name, bytesText(from), bytesText(s.RetainedBytes)))
		if s.RetainedBytes >= slotRetainedMin {
			h.add(0.2, ev, "it retains "+bytesText(s.RetainedBytes)+" of WAL")
		}
	}
	return h
}

// fastestGrowingActive is the active slot whose retained WAL grew most
// between the samples (growth 0 when nothing is comparable).
func fastestGrowingActive(now, before []probes.Slot) (probes.Slot, float64, float64) {
	var best probes.Slot
	bestFrom, bestGrowth := 0.0, math.Inf(-1)
	for _, s := range now {
		b, ok := slotByName(before, s.Name)
		if !s.Active || !ok || !probes.Known(s.RetainedBytes) ||
			!probes.Known(b.RetainedBytes) {
			continue
		}
		if g := s.RetainedBytes - b.RetainedBytes; g > bestGrowth {
			best, bestFrom, bestGrowth = s, b.RetainedBytes, g
		}
	}
	if math.IsInf(bestGrowth, -1) {
		s, _ := largest(now, true)
		return s, s.RetainedBytes, 0
	}
	return best, bestFrom, bestGrowth
}

func scoreArchiver(w walEvidence) Hypothesis {
	h := newHypothesis(ArchiverFailure, "archiver")
	if w.n == 0 {
		return h
	}
	last, err := probes.ArchiverStats(w.last.Result)
	if err != nil {
		return h
	}
	ev := w.last.EvidenceID
	if last.Mode == "off" {
		h.contradict(ev, "archive_mode is off")
		return h
	}
	failing := !last.LastFailedAt.IsZero() && (last.LastArchivedAt.IsZero() ||
		last.LastFailedAt.After(last.LastArchivedAt))
	if failing {
		h.add(0.5, ev, "the last archive attempt failed at "+
			probes.FormatValue(last.LastFailedAt)+", after the last success")
	}
	rose := false
	if first, err := probes.ArchiverStats(w.first.Result); err == nil && w.n > 1 &&
		first.StatsReset.Equal(last.StatsReset) && probes.Known(first.Failed) &&
		probes.Known(last.Failed) && last.Failed > first.Failed {
		rose = true
		h.add(0.2, ev, fmt.Sprintf("failed_count rose from %s to %s",
			probes.FormatValue(first.Failed), probes.FormatValue(last.Failed)))
	}
	if !failing && !rose {
		h.contradict(ev, "the last archive attempt succeeded")
	}
	return h
}

func scoreWriteSurge(r walRate) Hypothesis {
	h := newHypothesis(WriteSurge, "WAL volume")
	if !r.known {
		return h
	}
	avgKnown := probes.Known(r.avg) && r.avg > 0
	switch {
	case r.rate >= surgeMinRate && (!avgKnown || r.rate >= surgeRatio*r.avg):
		text := rateText(r)
		if avgKnown {
			text += fmt.Sprintf(", %s times its average of %s bytes/s since the "+
				"statistics reset", probes.FormatValue(math.Round(r.rate/r.avg)),
				probes.FormatValue(math.Round(r.avg)))
		}
		h.add(0.5, r.ev, text)
		if avgKnown && r.rate >= strongSurgeRatio*r.avg {
			h.add(0.2, r.ev, "that is at least 10 times its long-run rate")
		}
	case avgKnown && r.rate < calmRatio*r.avg:
		h.contradict(r.ev, rateText(r)+fmt.Sprintf(", within 1.5 times its average of "+
			"%s bytes/s", probes.FormatValue(math.Round(r.avg))))
	case !avgKnown && r.rate < surgeMinRate:
		h.contradict(r.ev, rateText(r)+", under the surge floor of 4194304 bytes/s")
	}
	return h
}
