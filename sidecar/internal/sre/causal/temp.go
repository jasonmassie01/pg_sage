package causal

import (
	"fmt"
	"math"
	"sort"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Temp-file explosion family: a runaway query holding a large live
// spill (temp_file_holders), one statement spilling on every call, or
// many statements each spilling a little (temp_spill_statements, i.e.
// pg_stat_statements). pg_stat_database's temp counters (temp_file_
// activity) rule both finished-spill hypotheses out when no temp file
// was written. Cumulative counters are never read as current activity.

// Temp-file thresholds.
const (
	runawayMinBytes       = 64 << 20
	dominantShare         = 0.5
	repeatedMinBytes      = 32 << 20
	repeatedMinCalls      = 2
	workloadMinStatements = 3
	workMemSmallMultiple  = 16
	workMemHugeMultiple   = 64
)

// tempWindow compares the first and last temp_file_activity samples.
type tempWindow struct {
	ev            string
	last          probes.TempStat
	hasLast       bool
	valid         bool
	span          float64
	dFiles, dByte float64
}

// spillDelta is one statement's temp writes between samples.
type spillDelta struct {
	qid          int64
	bytes, calls float64
}

// spillWindow compares the first and last temp_spill_statements samples.
type spillWindow struct {
	ev     string
	valid  bool
	deltas []spillDelta // growing statements, most bytes first
	total  float64
}

// DiagnoseTempFiles scores the temp-file explosion hypotheses.
func DiagnoseTempFiles(obs []Observation) Diagnosis {
	actSer, m1 := usableSeries(obs, probes.TempFileActivity)
	holdSer, m2 := usableSeries(obs, probes.TempFileHolders)
	spillSer, m3 := usableSeries(obs, probes.TempSpillStatements)
	act, r1 := compareTemp(actSer)
	spill, r2 := compareSpills(spillSer)
	missing := append(append(m1, m2...), m3...)
	missing = appendReason(missing, probes.TempFileActivity, r1)
	missing = appendReason(missing, probes.TempSpillStatements, r2)
	d := rank(FamilyTempFiles, []Hypothesis{scoreRunaway(holdSer),
		scoreRepeated(act, spill), scoreWorkMem(act, spill)})
	d.Missing = missing
	d.Observed = tempObserved(act)
	return d
}

func appendReason(ms []Missing, id probes.ID, reason string) []Missing {
	if reason == "" {
		return ms
	}
	return append(ms, Missing{ProbeID: id, Reason: reason})
}

func compareTemp(ser []Observation) (tempWindow, string) {
	var t tempWindow
	if len(ser) == 0 {
		return t, ""
	}
	first, last := ser[0], ser[len(ser)-1]
	b, err := probes.TempStats(last.Result)
	if err != nil {
		return t, "unreadable_sample"
	}
	t.ev, t.last, t.hasLast = last.EvidenceID, b, true
	if len(ser) < 2 {
		return t, "second_sample_unavailable"
	}
	a, err := probes.TempStats(first.Result)
	if err != nil {
		return t, "unreadable_sample"
	}
	var ok1, ok2 bool
	t.dFiles, ok1 = probes.Delta(a.Files, b.Files)
	t.dByte, ok2 = probes.Delta(a.Bytes, b.Bytes)
	if !ok1 || !ok2 || !a.StatsReset.Equal(b.StatsReset) ||
		!a.ServerStartedAt.Equal(b.ServerStartedAt) {
		return t, "counter_reset"
	}
	t.span, t.valid = spanSeconds(first, last), true
	return t, ""
}

func compareSpills(ser []Observation) (spillWindow, string) {
	var w spillWindow
	if len(ser) < 2 {
		if len(ser) == 1 {
			return w, "second_sample_unavailable"
		}
		return w, ""
	}
	first, last := ser[0], ser[len(ser)-1]
	a, errA := probes.SpillStatements(first.Result)
	b, errB := probes.SpillStatements(last.Result)
	if errA != nil || errB != nil {
		return w, "unreadable_sample"
	}
	if len(a) > 0 && len(b) > 0 && !a[0].StatsReset.Equal(b[0].StatsReset) {
		return w, "counter_reset"
	}
	before := map[int64]probes.SpillStatement{}
	for _, s := range a {
		before[s.QueryID] = s
	}
	w.ev = last.EvidenceID
	for _, s := range b {
		p := before[s.QueryID]
		blks, ok := probes.Delta(p.TempBlksWritten, s.TempBlksWritten)
		if !ok || s.Calls < p.Calls {
			return spillWindow{}, "counter_reset"
		}
		if bytes := blks * s.BlockSize; bytes > 0 {
			w.deltas = append(w.deltas, spillDelta{qid: s.QueryID, bytes: bytes,
				calls: float64(s.Calls - p.Calls)})
			w.total += bytes
		}
	}
	sort.SliceStable(w.deltas, func(i, j int) bool { return w.deltas[i].bytes > w.deltas[j].bytes })
	w.valid = true
	return w, ""
}

func scoreRunaway(ser []Observation) Hypothesis {
	h := newHypothesis(RunawaySpillQuery, "live temp files")
	if len(ser) == 0 {
		return h
	}
	last := ser[len(ser)-1]
	all, err := probes.TempHolders(last.Result)
	if err != nil {
		return h
	}
	mine := currentDBHolders(all)
	switch {
	case len(all) == 0:
		h.contradict(last.EvidenceID, "no backend held temp files at the last sample")
		return h
	case len(mine) == 0:
		h.contradict(last.EvidenceID, fmt.Sprintf("no backend of this database held temp "+
			"files (%d of other databases did)", len(all)))
		return h
	}
	top := mine[0]
	h.Subject = fmt.Sprintf("pid %d", top.PID)
	if top.Bytes < runawayMinBytes {
		return h
	}
	h.add(0.4, last.EvidenceID, holderText(top))
	var total float64
	for _, x := range mine {
		total += x.Bytes
	}
	if top.Bytes >= dominantShare*total {
		h.add(0.2, last.EvidenceID, fmt.Sprintf("that is %s%% of this database's live temp "+
			"files", pct(top.Bytes/total)))
	}
	if grew, from := holderGrew(ser[0], top); len(ser) > 1 && grew {
		h.add(0.1, last.EvidenceID, fmt.Sprintf("its temp files grew from %s to %s bytes "+
			"between samples", fv(from), fv(top.Bytes)))
	}
	return h
}

func currentDBHolders(all []probes.TempHolder) []probes.TempHolder {
	var mine []probes.TempHolder
	for _, x := range all {
		if x.InCurrentDatabase {
			mine = append(mine, x)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].Bytes > mine[j].Bytes })
	return mine
}

func holderText(x probes.TempHolder) string {
	text := fmt.Sprintf("pid %d holds %s bytes in %d temp files (state %s", x.PID,
		fv(x.Bytes), x.Files, probes.FormatValue(x.State))
	if x.QueryIDKnown {
		text += fmt.Sprintf(", queryid %d", x.QueryID)
	}
	if probes.Known(x.QueryAgeS) {
		text += fmt.Sprintf(", statement running %s s", fv(math.Round(x.QueryAgeS)))
	}
	return text + ")"
}

// holderGrew reports whether the same backend held fewer bytes in the
// first sample.
func holderGrew(first Observation, top probes.TempHolder) (bool, float64) {
	before, err := probes.TempHolders(first.Result)
	if err != nil {
		return false, 0
	}
	for _, x := range before {
		if x.PID == top.PID && x.BackendStart.Equal(top.BackendStart) {
			return x.Bytes < top.Bytes, x.Bytes
		}
	}
	return false, 0
}

// noTempWritten contradicts h when pg_stat_database saw no temp file
// written between samples.
func noTempWritten(h *Hypothesis, act tempWindow) bool {
	if act.valid && act.dFiles == 0 {
		h.contradict(act.ev, fmt.Sprintf("no temp file was written in %s s",
			fv(act.span)))
		return true
	}
	return false
}

func scoreRepeated(act tempWindow, w spillWindow) Hypothesis {
	h := newHypothesis(RepeatedSpillStatement, "spilling statements")
	if noTempWritten(&h, act) || !w.valid {
		return h
	}
	if w.total == 0 {
		h.contradict(w.ev, "no statement finished writing temp blocks between samples")
		return h
	}
	top := w.deltas[0]
	h.Subject = fmt.Sprintf("queryid %d", top.qid)
	if top.bytes < repeatedMinBytes || top.calls < repeatedMinCalls {
		return h
	}
	h.add(0.4, w.ev, fmt.Sprintf("queryid %d wrote %s bytes of temp files in %s calls "+
		"between samples", top.qid, fv(top.bytes), fv(top.calls)))
	if top.bytes >= dominantShare*w.total {
		h.add(0.2, w.ev, fmt.Sprintf("that is %s%% of the temp blocks statements wrote",
			pct(top.bytes/w.total)))
	}
	return h
}

func scoreWorkMem(act tempWindow, w spillWindow) Hypothesis {
	h := newHypothesis(WorkMemUndersized, "work_mem")
	if noTempWritten(&h, act) {
		return h
	}
	workMem := act.last.WorkMemBytes
	avg, avgKnown := 0.0, act.valid && act.dFiles > 0 && probes.Known(workMem) && workMem > 0
	if avgKnown {
		avg = math.Round(act.dByte / act.dFiles)
		if avg >= workMemHugeMultiple*workMem {
			h.contradict(act.ev, fmt.Sprintf("temp files averaged %s bytes, over 64 times "+
				"work_mem (%s bytes): a larger work_mem would not avoid these spills",
				fv(avg), fv(workMem)))
			return h
		}
	}
	if !w.valid {
		return h
	}
	switch top := firstDelta(w); {
	case w.total == 0:
		h.contradict(w.ev, "no statement finished writing temp blocks between samples")
	case top.bytes >= dominantShare*w.total:
		h.contradict(w.ev, fmt.Sprintf("queryid %d wrote %s%% of the temp blocks: one "+
			"statement, not the workload", top.qid, pct(top.bytes/w.total)))
	case len(w.deltas) >= workloadMinStatements:
		h.add(0.4, w.ev, fmt.Sprintf("%d statements wrote temp files between samples; none "+
			"wrote half of them", len(w.deltas)))
		if avgKnown && avg <= workMemSmallMultiple*workMem {
			h.add(0.2, act.ev, fmt.Sprintf("temp files averaged %s bytes, within 16 times "+
				"work_mem (%s bytes)", fv(avg), fv(workMem)))
		}
	}
	return h
}

func firstDelta(w spillWindow) spillDelta {
	if len(w.deltas) == 0 {
		return spillDelta{}
	}
	return w.deltas[0]
}

// tempObserved states the database's temp writes and its guardrails.
func tempObserved(act tempWindow) []Fact {
	var out []Fact
	if act.valid {
		out = append(out, Fact{EvidenceID: act.ev, Text: fmt.Sprintf("this database wrote "+
			"%s temp files (%s bytes) in %s s", fv(act.dFiles), fv(act.dByte), fv(act.span))})
	}
	if !act.hasLast {
		return out
	}
	if act.last.TempFileLimitKB == -1 {
		out = append(out, Fact{EvidenceID: act.ev, Text: "temp_file_limit is -1: no " +
			"session's temp files are capped"})
	}
	if act.last.TempTablespacesSet {
		out = append(out, Fact{EvidenceID: act.ev, Text: "temp_tablespaces is set: live " +
			"temp files outside the default tablespace are not visible"})
	}
	return out
}
