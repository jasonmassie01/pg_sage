package runway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-10: a reading lists the sequences nearest their limit, plus
// every sequence that already has samples in the lookback (at most
// probes.MaxSequenceKeep), so a sampled sequence never loses its series
// by falling out of the top of the list.

// keepRows answers a sequence read like sequence_runway v4: the n
// sequences s00.. (s00 nearest its limit), truncated to the top
// probes.SequenceTopN, plus any other of them on the keep-list.
func keepRows(n int, a probes.Args) probes.Result {
	keep := map[string]bool{}
	for _, k := range a.Keep {
		keep[k] = true
	}
	res := probes.Result{ProbeID: probes.SequenceRunwayProbe, Status: probes.StatusOK,
		ObservedAt: time.Now()}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("public.s%02d", i)
		if i >= probes.SequenceTopN && !keep[name] {
			continue
		}
		res.Rows = append(res.Rows, probes.Row{"sequence": name, "data_type": "bigint",
			"increment_by": int64(1), "last_value": int64(990 - i), "min_value": int64(1),
			"max_value": int64(1000), "effective_limit": int64(1000),
			"fraction_used": float64(990-i) / 1000, "cycle": false,
			"sequences_total": int64(n), "sequences_scanned": int64(n),
			"sequences_used": int64(n), "sequences_unreadable": int64(0)})
	}
	return res
}

func sequenceTrends(names ...string) probes.RunwayTrendSet {
	var set probes.RunwayTrendSet
	for i, n := range names {
		set.Trends = append(set.Trends, probes.RunwayTrend{Kind: probes.RunwaySequence,
			Subject: n, Samples: 3, LastValue: 900, Limit: 1000, RatePerS: float64(i + 1)})
	}
	set.Trends = append(set.Trends, probes.RunwayTrend{Kind: probes.RunwayXID,
		Subject: probes.SubjectCluster, Samples: 3})
	return set
}

func names(ss []probes.SequenceRunway) map[string]bool {
	out := map[string]bool{}
	for _, s := range ss {
		out[s.Sequence] = true
	}
	return out
}

func TestMonitorRead_ASampledSequenceAtRank51IsStillSampled(t *testing.T) {
	total := probes.SequenceTopN // the first pass: low is the 50th of 50
	r := newCadenceRunner(probes.Result{})
	r.bySlice = func(a probes.Args) probes.Result { return keepRows(total, a) }
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	m := cadenceMonitor(r, 0, c, &logSink{})
	low := fmt.Sprintf("public.s%02d", probes.SequenceTopN-1)
	snap, _ := m.read(context.Background())
	if !names(snap.Sequences)[low] {
		t.Fatalf("first pass lists %d sequences without %s", len(snap.Sequences), low)
	}
	if len(r.seqArgs[0].Keep) != 0 {
		t.Fatalf("first pass kept %v, want nothing before any series", r.seqArgs[0].Keep)
	}
	m.noteSequenceSeries(sequenceTrends(low)) // its sample is in the lookback
	// Two hotter sequences appear: low is now 52nd and s51 (no samples) 51st.
	total = probes.SequenceTopN + 2
	low2 := fmt.Sprintf("public.s%02d", probes.SequenceTopN+1)
	m.noteSequenceSeries(sequenceTrends(low2))
	snap, _ = m.read(context.Background())
	keep := r.seqArgs[len(r.seqArgs)-1].Keep
	if len(keep) != 1 || keep[0] != low2 {
		t.Fatalf("second pass keep-list = %v, want [%s]", keep, low2)
	}
	got := names(snap.Sequences)
	if !got[low2] || got[fmt.Sprintf("public.s%02d", probes.SequenceTopN)] ||
		len(snap.Sequences) != probes.SequenceTopN+1 {
		t.Fatalf("second pass listed %d (%v), want the top %d plus the kept %s",
			len(snap.Sequences), got, probes.SequenceTopN, low2)
	}
	if _, ok := byKey(BuildSamples(snap, m.opts))["sequence/"+low2]; !ok {
		t.Fatalf("the kept sequence %s was not sampled", low2)
	}
}

// The keep-list is bounded: the MaxSequenceKeep series nearest their
// limit (soonest projection) are kept; other kinds never are.
func TestMonitor_KeepListIsBounded(t *testing.T) {
	m := cadenceMonitor(newCadenceRunner(probes.Result{}), 0,
		&clock{t: time.Now()}, &logSink{})
	var subjects []string
	for i := 0; i < probes.MaxSequenceKeep+10; i++ {
		subjects = append(subjects, fmt.Sprintf("public.k%03d", i))
	}
	m.noteSequenceSeries(sequenceTrends(subjects...))
	keep := m.sequenceKeep()
	if len(keep) != probes.MaxSequenceKeep {
		t.Fatalf("keep-list holds %d, want %d", len(keep), probes.MaxSequenceKeep)
	}
	// sequenceTrends gives later subjects faster rates: the last ones are
	// the soonest to their limit.
	if keep[0] != subjects[len(subjects)-1] || contains(keep, subjects[0]) ||
		contains(keep, probes.SubjectCluster) {
		t.Fatalf("keep-list = %v, want the soonest series first and no xid series", keep)
	}
	m.noteSequenceSeries(probes.RunwayTrendSet{}) // no sequence series left
	if keep := m.sequenceKeep(); len(keep) != 0 {
		t.Fatalf("keep-list = %v after the series aged out, want empty", keep)
	}
}

// A runway_trends read cut by its row cap may lack sequence series: the
// sequences kept before stay kept (fail closed: no series is dropped).
func TestMonitor_ACutTrendsReadForgetsNoSeries(t *testing.T) {
	m := cadenceMonitor(newCadenceRunner(probes.Result{}), 0,
		&clock{t: time.Now()}, &logSink{})
	m.noteSequenceSeries(sequenceTrends("public.a", "public.b"))
	cut := sequenceTrends("public.c")
	cut.Truncated, cut.LastKind = true, probes.RunwaySequence
	m.noteSequenceSeries(cut)
	keep := m.sequenceKeep()
	if len(keep) != 3 || keep[0] != "public.c" || !contains(keep, "public.a") ||
		!contains(keep, "public.b") {
		t.Fatalf("keep-list after a cut read = %v, want c then a and b", keep)
	}
	m.noteSequenceSeries(sequenceTrends("public.c")) // a complete read replaces it
	if keep := m.sequenceKeep(); len(keep) != 1 || keep[0] != "public.c" {
		t.Fatalf("keep-list after a complete read = %v, want [public.c]", keep)
	}
}
