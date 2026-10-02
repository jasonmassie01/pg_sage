package causal

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Checkpoint storm family: requested checkpoints driven by WAL volume
// (max_wal_size too small), forced checkpoints, a short
// checkpoint_timeout, and a write burst that only amplifies requested
// checkpoints. The first and last checkpoint_activity samples are
// compared; a reset or restart between them makes every delta unknown.

// Checkpoint thresholds.
const (
	// ckptVolumeShare: requested checkpoints with at least this share of
	// max_wal_size of WAL each were requested by WAL volume. PostgreSQL
	// requests one at max_wal_size / (1 + checkpoint_completion_target),
	// about half of it, so a quarter leaves margin for partial windows.
	ckptVolumeShare = 0.25
	// ckptDefaultTimeoutS is checkpoint_timeout's default.
	ckptDefaultTimeoutS = 300
)

// ckptWindow is the comparison of the first and last samples.
type ckptWindow struct {
	ev      string // the last sample's evidence id
	hasLast bool
	a, b    probes.CheckpointStat
	valid   bool
	span    float64
	dReq    float64
	dTimed  float64
	dWAL    float64
	avg     float64 // long-run WAL rate since its reset; NaN when unknown
}

// DiagnoseCheckpoint scores the checkpoint storm hypotheses.
func DiagnoseCheckpoint(obs []Observation) Diagnosis {
	ser, missing := usableSeries(obs, probes.CheckpointActivity)
	w, reason := compareCkpt(ser)
	if reason != "" {
		missing = append(missing, Missing{ProbeID: probes.CheckpointActivity, Reason: reason})
	}
	d := rank(FamilyCheckpoint, []Hypothesis{scoreUndersized(w), scoreForced(w),
		scoreShortTimeout(w), scoreCkptBurst(w)})
	d.Missing = missing
	d.Observed = ckptObserved(w)
	return d
}

func compareCkpt(ser []Observation) (ckptWindow, string) {
	w := ckptWindow{avg: math.NaN()}
	if len(ser) == 0 {
		return w, ""
	}
	first, last := ser[0], ser[len(ser)-1]
	b, err := probes.CheckpointStats(last.Result)
	if err != nil {
		return w, "unreadable_sample"
	}
	w.ev, w.hasLast, w.b = last.EvidenceID, true, b
	if len(ser) < 2 {
		return w, "second_sample_unavailable"
	}
	a, err := probes.CheckpointStats(first.Result)
	if err != nil {
		return w, "unreadable_sample"
	}
	w.a = a
	if !a.StatsReset.Equal(b.StatsReset) || !a.WALStatsReset.Equal(b.WALStatsReset) ||
		!a.ServerStartedAt.Equal(b.ServerStartedAt) {
		return w, "counter_reset"
	}
	var ok1, ok2, ok3 bool
	w.dReq, ok1 = probes.Delta(a.Requested, b.Requested)
	w.dTimed, ok2 = probes.Delta(a.Timed, b.Timed)
	w.dWAL, ok3 = probes.Delta(a.WALBytes, b.WALBytes)
	if !ok1 || !ok2 || !ok3 {
		return w, "counter_reset"
	}
	if w.span = spanSeconds(first, last); w.span <= 0 {
		return w, "samples_out_of_order"
	}
	if age := last.Result.ObservedAt.Sub(b.WALStatsReset).Seconds(); !b.WALStatsReset.IsZero() &&
		age > 0 {
		w.avg = b.WALBytes / age
	}
	w.valid = true
	return w, ""
}

func (w ckptWindow) rate() float64 { return w.dWAL / w.span }

// perRequest is the WAL written per requested checkpoint and whether
// that classifies them (max_wal_size known, at least one request).
func (w ckptWindow) perRequest() (float64, bool) {
	if !w.valid || w.dReq < 1 || !probes.Known(w.b.MaxWALSize) || w.b.MaxWALSize <= 0 {
		return 0, false
	}
	return w.dWAL / w.dReq, true
}

func (w ckptWindow) volumeDriven(per float64) bool {
	return per >= ckptVolumeShare*w.b.MaxWALSize
}

// fill is how long the measured WAL rate takes to reach the checkpoint
// distance, max_wal_size / (1 + checkpoint_completion_target).
func (w ckptWindow) fill() (distance, seconds float64, ok bool) {
	if !w.valid || !probes.Known(w.b.MaxWALSize) || !probes.Known(w.b.CompletionTarget) ||
		!probes.Known(w.b.TimeoutS) || w.b.MaxWALSize <= 0 {
		return 0, 0, false
	}
	distance = math.Round(w.b.MaxWALSize / (1 + w.b.CompletionTarget))
	if r := w.rate(); r > 0 {
		return distance, math.Round(distance / r), true
	}
	return distance, math.Inf(1), true
}

func requestedText(w ckptWindow, per float64, qualifier string) string {
	share := "under"
	if w.volumeDriven(per) {
		share = "at least"
	}
	return fmt.Sprintf("%s checkpoints were requested in %s s with %s%s bytes of WAL each "+
		"on average, %s a quarter of max_wal_size (%s bytes)", fv(w.dReq), fv(w.span),
		qualifier, fv(math.Round(per)), share, fv(w.b.MaxWALSize))
}

func scoreUndersized(w ckptWindow) Hypothesis {
	h := newHypothesis(MaxWALSizeUndersized, "max_wal_size")
	per, ok := w.perRequest()
	if ok && !w.volumeDriven(per) {
		h.contradict(w.ev, fmt.Sprintf("%s requested checkpoints came with %s bytes of WAL "+
			"each, under a quarter of max_wal_size (%s bytes): WAL volume did not request "+
			"them", fv(w.dReq), fv(math.Round(per)), fv(w.b.MaxWALSize)))
		return h
	}
	if ok {
		h.add(0.4, w.ev, requestedText(w, per, ""))
		if w.dReq >= 2 {
			h.add(0.1, w.ev, fmt.Sprintf("that is one checkpoint every %s s",
				fv(math.Round(w.span/w.dReq))))
		}
	}
	distance, secs, known := w.fill()
	switch {
	case known && secs < w.b.TimeoutS:
		h.add(0.25, w.ev, fmt.Sprintf("at %s bytes/s of WAL the checkpoint distance of %s "+
			"bytes (max_wal_size / (1 + checkpoint_completion_target)) fills in %s s, "+
			"sooner than checkpoint_timeout (%s s)", fv(math.Round(w.rate())), fv(distance),
			fv(secs), fv(w.b.TimeoutS)))
	case known && w.dReq == 0:
		h.contradict(w.ev, fmt.Sprintf("no checkpoint was requested in %s s, and at %s "+
			"bytes/s of WAL the checkpoint distance of %s bytes outlasts checkpoint_timeout "+
			"(%s s)", fv(w.span), fv(math.Round(w.rate())), fv(distance), fv(w.b.TimeoutS)))
	}
	return h
}

func scoreForced(w ckptWindow) Hypothesis {
	h := newHypothesis(ForcedCheckpoints, "requested checkpoints")
	if !w.valid {
		return h
	}
	if w.dReq == 0 {
		h.contradict(w.ev, fmt.Sprintf("no checkpoint was requested in %s s", fv(w.span)))
		return h
	}
	per, ok := w.perRequest()
	switch {
	case !ok:
	case w.volumeDriven(per):
		h.contradict(w.ev, fmt.Sprintf("the requested checkpoints came with %s bytes of WAL "+
			"each, at least a quarter of max_wal_size: WAL volume requested them",
			fv(math.Round(per))))
	default:
		h.add(0.5, w.ev, requestedText(w, per, "only "))
		if w.dReq >= 3 {
			h.add(0.2, w.ev, fmt.Sprintf("that is one checkpoint every %s s",
				fv(math.Round(w.span/w.dReq))))
		}
	}
	return h
}

func scoreShortTimeout(w ckptWindow) Hypothesis {
	h := newHypothesis(ShortCheckpointTimeout, "checkpoint_timeout")
	if !w.hasLast || !probes.Known(w.b.TimeoutS) {
		return h
	}
	if w.b.TimeoutS >= ckptDefaultTimeoutS {
		h.contradict(w.ev, fmt.Sprintf("checkpoint_timeout is %s s, the 300 s default or "+
			"longer", fv(w.b.TimeoutS)))
		return h
	}
	h.add(0.2, w.ev, fmt.Sprintf("checkpoint_timeout is %s s, under the 300 s default",
		fv(w.b.TimeoutS)))
	if w.valid && w.dTimed >= 1 {
		h.add(0.5, w.ev, fmt.Sprintf("%s timed checkpoints ran in %s s", fv(w.dTimed),
			fv(w.span)))
	}
	return h
}

func scoreCkptBurst(w ckptWindow) Hypothesis {
	h := newHypothesis(CheckpointWriteBurst, "WAL volume")
	if !w.valid {
		return h
	}
	if w.dReq == 0 {
		h.contradict(w.ev, fmt.Sprintf("no checkpoint was requested in %s s: the writes "+
			"fit within max_wal_size", fv(w.span)))
		return h
	}
	burstScore(&h, w.ev, w.rate(), w.span, w.avg)
	return h
}

// ckptObserved states the WAL rate, backend fsyncs and the full-page
// image share of the window.
func ckptObserved(w ckptWindow) []Fact {
	if !w.valid {
		return nil
	}
	out := []Fact{{EvidenceID: w.ev, Text: fmt.Sprintf("WAL was written at %s bytes/s "+
		"over %s s; %s requested and %s timed checkpoints ran", fv(math.Round(w.rate())),
		fv(w.span), fv(w.dReq), fv(w.dTimed))}}
	if d, ok := probes.Delta(w.a.BackendFsyncs, w.b.BackendFsyncs); ok && d > 0 {
		out = append(out, Fact{EvidenceID: w.ev, Text: fmt.Sprintf("client backends "+
			"fsynced %s buffers themselves in %s s: the checkpointer did not absorb them",
			fv(d), fv(w.span))})
	}
	recs, ok1 := probes.Delta(w.a.WALRecords, w.b.WALRecords)
	fpi, ok2 := probes.Delta(w.a.WALFPI, w.b.WALFPI)
	if ok1 && ok2 && recs > 0 {
		out = append(out, Fact{EvidenceID: w.ev, Text: fmt.Sprintf("full-page images were "+
			"%s%% of the %s WAL records written", pct(fpi/recs), fv(recs))})
	}
	return out
}
