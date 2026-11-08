package runway

import "github.com/pg-sage/sidecar/internal/sre/probes"

// Why a series' open finding is held (kept open and marked stale)
// instead of resolved: no runway was projected, but nothing measured
// the series either, which is not a cleared runway.
const (
	// HoldUnmeasured: the series has samples in the lookback, but too
	// few, over too short a span, or with an unknown rate.
	HoldUnmeasured = "unmeasured"
	// HoldNoSamples: the series has no sample in the lookback at all.
	HoldNoSamples = "no_samples"
)

// categoryKinds are the series kinds whose trends each category's
// findings are projected from.
var categoryKinds = map[string][]string{
	CategoryWraparound: {probes.RunwayXID, probes.RunwayMXID},
	CategoryWAL:        {probes.RunwayDiskUsed, probes.RunwayWALSlot},
	CategorySequence:   {probes.RunwaySequence},
}

// kindComplete reports whether the trends hold every series of kind: a
// truncated read lacks what sorts at or after the last kind it returned.
func (in EvalInput) kindComplete(kind string) bool {
	return probes.RunwayTrendSet{Truncated: in.TrendsTruncated,
		LastKind: in.TrendsLastKind}.Complete(kind)
}

// trendsComplete reports whether the trends hold every series of cat.
func (in EvalInput) trendsComplete(cat string) bool {
	for _, kind := range categoryKinds[cat] {
		if !in.kindComplete(kind) {
			return false
		}
	}
	return true
}

// Holds returns, per category, the finding identifiers whose series this
// tick could not measure, with the reason. Resolving treats them as
// active: an unmeasured series keeps its finding open.
//   - The cluster series (xid, mxid, disk) are held when unmeasured, or
//     when they have no row in a complete read.
//   - A slot or sequence is held only while the current reading still
//     lists it, so a dropped one (or a sequence below the top N) may
//     resolve. While that reading is unreadable, every unmeasured row is
//     held: the object may still exist.
//
// Unreadable trends hold nothing (nil); nothing is evaluated then either.
func Holds(in EvalInput, opts Options) map[string]map[string]string {
	if !in.TrendsOK {
		return nil
	}
	held := map[string]map[string]string{}
	rows := map[string]map[string]bool{} // kind -> subject -> measured
	for _, tr := range in.Trends {
		if rows[tr.Kind] == nil {
			rows[tr.Kind] = map[string]bool{}
		}
		rows[tr.Kind][tr.Subject] = measured(tr, opts)
	}
	hold := func(cat, ident, reason string) {
		if held[cat] == nil {
			held[cat] = map[string]string{}
		}
		held[cat][ident] = reason
	}
	cluster := []struct{ kind, cat, ident string }{
		{probes.RunwayXID, CategoryWraparound, "xid"},
		{probes.RunwayMXID, CategoryWraparound, "mxid"},
		{probes.RunwayDiskUsed, CategoryWAL, "disk"}}
	for _, c := range cluster {
		if r := in.reason(rows, c.kind, probes.SubjectCluster); r != "" {
			hold(c.cat, c.ident, r)
		}
	}
	var slots, seqs []string
	for _, s := range in.Slots {
		slots = append(slots, s.Name)
	}
	for _, s := range in.Sequences {
		if !s.Cycle { // a cycling sequence never runs out
			seqs = append(seqs, s.Sequence)
		}
	}
	in.holdObjects(rows, probes.RunwayWALSlot, slots, in.SlotsOK, func(n, r string) {
		hold(CategoryWAL, "slot:"+n, r)
	})
	in.holdObjects(rows, probes.RunwaySequence, seqs, in.SequencesOK, func(n, r string) {
		hold(CategorySequence, n, r)
	})
	return held
}

// reason is why the series (kind, subject) is held, "" when it is not:
// measured, or absent from a read that may have cut it.
func (in EvalInput) reason(rows map[string]map[string]bool, kind, subject string) string {
	m, ok := rows[kind][subject]
	switch {
	case ok && !m:
		return HoldUnmeasured
	case !ok && in.kindComplete(kind):
		return HoldNoSamples
	}
	return ""
}

// holdObjects holds the listed objects of kind that are not measured
// when their reading is readable, else every unmeasured row of kind.
func (in EvalInput) holdObjects(rows map[string]map[string]bool, kind string,
	listed []string, readOK bool, hold func(name, reason string)) {
	if !readOK {
		for subject, m := range rows[kind] {
			if !m {
				hold(subject, HoldUnmeasured)
			}
		}
		return
	}
	for _, name := range listed {
		if r := in.reason(rows, kind, name); r != "" {
			hold(name, r)
		}
	}
}
