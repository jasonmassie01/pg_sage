package causal

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Replication lag family. On a primary, the most lagging replica's lag
// (pg_stat_replication) is split by where it sits: WAL not yet sent,
// sent but not flushed by the standby, flushed but not replayed. The
// stage holding most of it is the mechanism; a primary write surge only
// amplifies it. On a standby, standby_replay_state shows the replay
// backlog and why replay is held back (paused, or standby queries);
// each side's evidence is unavailable from the other, and says so.

// Replication thresholds.
const (
	lagMinBytes      = 16 << 20
	stageShare       = 0.5
	stageMinorShare  = 0.2
	lagTimeMinS      = 10
	standbyQueryMinS = 30
)

// replWindow is the replication evidence of one investigation.
type replWindow struct {
	lagOK       bool
	lagEv       string
	last, first []probes.ReplicationStage
	worst       probes.ReplicationStage
	standbyOK   bool
	inRecovery  bool
	stEv        string
	st, stFirst probes.StandbyState
	stN         int
	rate        walRate
}

// DiagnoseReplicationLag scores the replication lag hypotheses.
func DiagnoseReplicationLag(obs []Observation) Diagnosis {
	w, missing := replEvidence(obs)
	hs := []Hypothesis{scoreStage(w, WALSendBacklog), scoreStage(w, StandbyFlushBacklog),
		scoreReplayBacklog(w), scoreReplayPaused(w), scoreStandbyDelay(w), scoreReplSurge(w)}
	d := rank(FamilyReplicationLag, hs)
	d.Missing = missing
	d.Observed = replObserved(w)
	return d
}

func replEvidence(obs []Observation) (replWindow, []Missing) {
	w := replWindow{}
	lagSer, missing := usableSeries(obs, probes.ReplicationLag)
	if n := len(lagSer); n > 0 {
		last, errL := probes.ReplicationStages(lagSer[n-1].Result)
		first, errF := probes.ReplicationStages(lagSer[0].Result)
		w.lagOK = errL == nil && errF == nil
		w.lagEv, w.last, w.first = lagSer[n-1].EvidenceID, last, first
		w.worst = worstReplica(last)
	}
	stSer, m2 := usableSeries(obs, probes.StandbyReplayState)
	missing = append(missing, m2...)
	if n := len(stSer); n > 0 {
		last, errL := probes.StandbyStates(stSer[n-1].Result)
		first, errF := probes.StandbyStates(stSer[0].Result)
		w.standbyOK = errL == nil && errF == nil
		w.st, w.stFirst, w.stEv, w.stN = last, first, stSer[n-1].EvidenceID, n
		w.inRecovery = w.standbyOK && last.InRecovery
	}
	switch {
	case w.inRecovery:
		missing = append(missing, Missing{ProbeID: probes.ReplicationLag,
			Reason: "primary_side_unavailable"})
	case w.standbyOK:
		missing = append(missing, Missing{ProbeID: probes.StandbyReplayState,
			Reason: "not_a_standby"})
	}
	if !w.inRecovery {
		wal, m3 := walSeries(obs, probes.WALCheckpoint)
		changed, mID := walComparable(&walEvidence{}, &wal, &walEvidence{})
		var m4 []Missing
		w.rate, m4 = measureRate(wal, changed)
		missing = append(append(append(missing, m3...), mID...), m4...)
	}
	return w, missing
}

// worstReplica is the replica with the most total lag.
func worstReplica(rs []probes.ReplicationStage) probes.ReplicationStage {
	var best probes.ReplicationStage
	bestTotal := math.Inf(-1)
	for _, r := range rs {
		if t := totalLag(r); probes.Known(t) && t > bestTotal {
			best, bestTotal = r, t
		}
	}
	return best
}

// totalLag is the replay lag in bytes, or the sum of its known stages.
func totalLag(r probes.ReplicationStage) float64 {
	if probes.Known(r.ReplayLagBytes) {
		return r.ReplayLagBytes
	}
	sum, known := 0.0, false
	for _, b := range []float64{r.SendBacklog, r.FlushBacklog, r.ReplayBacklog} {
		if probes.Known(b) {
			sum, known = sum+b, true
		}
	}
	if !known {
		return math.NaN()
	}
	return sum
}

func replicaSubject(r probes.ReplicationStage) string {
	return fmt.Sprintf("replica %q (pid %d)", probes.FormatValue(r.Application), r.PID)
}

// lagStage is one place the lag can sit.
type lagStage struct {
	where   string
	bytes   func(probes.ReplicationStage) float64
	lagName string
	lag     func(probes.ReplicationStage) float64
}

var lagStages = map[NodeID]lagStage{
	WALSendBacklog: {"WAL not yet sent", func(r probes.ReplicationStage) float64 {
		return r.SendBacklog
	}, "", nil},
	StandbyFlushBacklog: {"between sent and flushed", func(r probes.ReplicationStage) float64 {
		return r.FlushBacklog
	}, "flush_lag", func(r probes.ReplicationStage) float64 { return r.FlushLagS }},
	StandbyReplayBacklog: {"between flushed and replayed",
		func(r probes.ReplicationStage) float64 { return r.ReplayBacklog }, "replay_lag",
		func(r probes.ReplicationStage) float64 { return r.ReplayLagS }},
}

// lagFloor contradicts h when no replica is connected or the worst one
// keeps up; it reports whether h was contradicted.
func lagFloor(h *Hypothesis, w replWindow) bool {
	if len(w.last) == 0 {
		h.contradict(w.lagEv, "no standby or replication client is connected")
		return true
	}
	if t := totalLag(w.worst); probes.Known(t) && t < lagMinBytes {
		h.contradict(w.lagEv, fmt.Sprintf("replica %q keeps up: it is %s bytes behind, "+
			"under 16777216", probes.FormatValue(w.worst.Application), fv(t)))
		return true
	}
	return false
}

func scoreStage(w replWindow, id NodeID) Hypothesis {
	h := newHypothesis(id, "replicas")
	if w.inRecovery || !w.lagOK || lagFloor(&h, w) {
		return h
	}
	r, s := w.worst, lagStages[id]
	h.Subject = replicaSubject(r)
	total, b := totalLag(r), s.bytes(r)
	if !probes.Known(total) || !probes.Known(b) {
		return h
	}
	share := math.Max(b, 0) / total
	switch {
	case share >= stageShare:
		h.add(0.5, w.lagEv, fmt.Sprintf("%s of the %s bytes of lag (%s%%) is %s", fv(b),
			fv(total), pct(share), s.where))
		stageGrowth(&h, w, s, b)
	case share < stageMinorShare:
		h.contradict(w.lagEv, fmt.Sprintf("only %s bytes (%s%%) of the lag is %s",
			fv(math.Max(b, 0)), pct(share), s.where))
	default:
		h.add(0.2, w.lagEv, fmt.Sprintf("%s bytes (%s%%) of the lag is %s", fv(b),
			pct(share), s.where))
	}
	return h
}

// stageGrowth adds growth since the first sample and a long lag time.
func stageGrowth(h *Hypothesis, w replWindow, s lagStage, b float64) {
	for _, f := range w.first {
		if f.PID == w.worst.PID && probes.Known(s.bytes(f)) && s.bytes(f) < b {
			h.add(0.15, w.lagEv, fmt.Sprintf("it grew from %s to %s bytes between samples",
				fv(s.bytes(f)), fv(b)))
			break
		}
	}
	if s.lag != nil {
		if t := s.lag(w.worst); probes.Known(t) && t >= lagTimeMinS {
			h.add(0.1, w.lagEv, fmt.Sprintf("%s is %s s", s.lagName, fv(t)))
		}
	}
}

func scoreReplayBacklog(w replWindow) Hypothesis {
	if !w.inRecovery {
		return scoreStage(w, StandbyReplayBacklog)
	}
	h := newHypothesis(StandbyReplayBacklog, "this standby")
	b := w.st.ReceiveReplayBytes
	switch {
	case !probes.Known(b):
	case b < lagMinBytes:
		h.contradict(w.stEv, fmt.Sprintf("the standby has replayed all but %s bytes of "+
			"the WAL it received", fv(b)))
	default:
		h.add(0.5, w.stEv, fmt.Sprintf("the standby has received %s bytes of WAL it has "+
			"not replayed", fv(b)))
		if a := w.stFirst.ReceiveReplayBytes; w.stN > 1 && probes.Known(a) && a < b {
			h.add(0.15, w.stEv, fmt.Sprintf("it grew from %s to %s bytes between samples",
				fv(a), fv(b)))
		}
	}
	return h
}

func scoreReplayPaused(w replWindow) Hypothesis {
	h := newHypothesis(ReplayPaused, "WAL replay")
	if !w.inRecovery {
		return h
	}
	if w.st.ReplayPaused {
		h.add(0.6, w.stEv, "WAL replay is paused on this standby")
	} else {
		h.contradict(w.stEv, "WAL replay is not paused")
	}
	return h
}

func scoreStandbyDelay(w replWindow) Hypothesis {
	h := newHypothesis(StandbyQueryDelay, "standby queries")
	if !w.inRecovery {
		return h
	}
	conflicts, confOK := probes.Delta(w.stFirst.Conflicts, w.st.Conflicts)
	confOK = confOK && w.stN > 1 && w.stFirst.ServerStartedAt.Equal(w.st.ServerStartedAt)
	delay := w.st.MaxStandbyDelayMS
	long := probes.Known(w.st.LongestQueryS) && w.st.LongestQueryS >= standbyQueryMinS &&
		probes.Known(delay) && (delay == -1 || delay >= standbyQueryMinS*1000)
	if confOK && conflicts > 0 {
		h.add(0.3, w.stEv, fmt.Sprintf("%s recovery conflicts cancelled standby queries "+
			"between samples", fv(conflicts)))
	}
	if long {
		h.add(0.3, w.stEv, fmt.Sprintf("a standby query has run %s s with "+
			"max_standby_streaming_delay at %s ms", fv(math.Round(w.st.LongestQueryS)),
			fv(delay)))
	}
	if confOK && conflicts == 0 && !long {
		h.contradict(w.stEv, "no recovery conflict between samples and no standby query "+
			"ran 30 s under a long max_standby_streaming_delay")
	}
	return h
}

func scoreReplSurge(w replWindow) Hypothesis {
	h := newHypothesis(ReplicationWriteSurge, "WAL volume")
	if w.inRecovery || (w.lagOK && lagFloor(&h, w)) || !w.rate.known {
		return h
	}
	burstScore(&h, w.rate.ev, w.rate.rate, w.rate.span, w.rate.avg)
	return h
}

// replObserved states the worst replica's lag by stage, or that this
// server is a standby.
func replObserved(w replWindow) []Fact {
	if w.inRecovery {
		return []Fact{{EvidenceID: w.stEv, Text: fmt.Sprintf("this server is a standby "+
			"(WAL receiver %s)", probes.FormatValue(w.st.ReceiverStatus))}}
	}
	r := w.worst
	if !w.lagOK || len(w.last) == 0 || !probes.Known(totalLag(r)) {
		return nil
	}
	return []Fact{{EvidenceID: w.lagEv, Text: fmt.Sprintf("%s replica %q is %s bytes "+
		"behind: %s not sent, %s sent but not flushed, %s flushed but not replayed",
		probes.FormatValue(r.Kind), probes.FormatValue(r.Application), fv(totalLag(r)),
		fv(r.SendBacklog), fv(r.FlushBacklog), fv(r.ReplayBacklog))}}
}
