package causal

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// LWLock contention family: lwlock_waits samples of active backends,
// classified by the LWLock they wait on. A class is supported when at
// least lwMinWaiters backends wait on it in at least half of the samples
// (sustained, not transient); one nobody waited on in any sample is
// ruled out. Waits on LWLocks no node models are reported, not forced
// into a class.

// LWLock thresholds.
const (
	lwMinWaiters  = 4
	lwMinSamples  = 2
	lwActiveShare = 4 // the peak is at least a quarter of active backends
)

// lwlockClasses maps wait event names (PostgreSQL 14 to 18, including
// the older lower-case and *ControlLock names) to their class.
var lwlockClasses = map[string]NodeID{
	"LockManager": LockManagerContention, "lock_manager": LockManagerContention,
	"LockFastPath": LockManagerContention,
	"SubtransSLRU": SubtransSLRUContention, "SubtransBuffer": SubtransSLRUContention,
	"SubtransControlLock":        SubtransSLRUContention,
	"MultiXactOffsetSLRU":        MultiXactSLRUContention,
	"MultiXactOffsetBuffer":      MultiXactSLRUContention,
	"MultiXactMemberSLRU":        MultiXactSLRUContention,
	"MultiXactMemberBuffer":      MultiXactSLRUContention,
	"MultiXactOffsetControlLock": MultiXactSLRUContention,
	"MultiXactMemberControlLock": MultiXactSLRUContention,
	"MultiXactGen":               MultiXactSLRUContention,
	"WALWrite":                   WALWriteContention, "WALInsert": WALWriteContention,
	"WALBufMapping": WALWriteContention, "WALWriteLock": WALWriteContention,
	"BufferMapping": BufferContention, "BufferContent": BufferContention,
	"buffer_mapping": BufferContention, "buffer_content": BufferContention,
}

// LWLockClass returns the class node of an LWLock wait event name.
func LWLockClass(event string) (NodeID, bool) {
	id, ok := lwlockClasses[event]
	return id, ok
}

var lwlockOrder = []NodeID{LockManagerContention, SubtransSLRUContention,
	MultiXactSLRUContention, WALWriteContention, BufferContention}

// lwSample is one decoded sample.
type lwSample struct {
	ev     string
	active int64
	class  map[NodeID]int64
	events map[string]int64 // every LWLock event's waiters
	query  map[NodeID]map[int64]int64
}

func newLWSample(o Observation, gs []probes.WaitGroup) lwSample {
	s := lwSample{ev: o.EvidenceID, class: map[NodeID]int64{}, events: map[string]int64{},
		query: map[NodeID]map[int64]int64{}}
	for _, g := range gs {
		s.active = max(s.active, g.ActiveBackends)
		if g.Type != "LWLock" {
			continue
		}
		s.events[g.Event] += g.Backends
		id, ok := LWLockClass(g.Event)
		if !ok {
			continue
		}
		s.class[id] += g.Backends
		if g.InCurrentDatabase && g.QueryIDKnown {
			if s.query[id] == nil {
				s.query[id] = map[int64]int64{}
			}
			s.query[id][g.QueryID] += g.Backends
		}
	}
	return s
}

// DiagnoseLWLock scores the LWLock contention hypotheses.
func DiagnoseLWLock(obs []Observation) Diagnosis {
	ser, missing := usableSeries(obs, probes.LWLockWaits)
	var ss []lwSample
	for _, o := range ser {
		gs, err := probes.WaitGroups(o.Result)
		if err != nil {
			missing = append(missing, Missing{ProbeID: probes.LWLockWaits,
				Reason: "unreadable_sample"})
			continue
		}
		ss = append(ss, newLWSample(o, gs))
	}
	if len(ss) > 0 && len(ss) < lwMinSamples {
		missing = append(missing, Missing{ProbeID: probes.LWLockWaits,
			Reason: "too_few_samples"})
	}
	span := 0.0
	if len(ser) > 1 {
		span = spanSeconds(ser[0], ser[len(ser)-1])
	}
	hs := make([]Hypothesis, 0, len(lwlockOrder))
	for _, id := range lwlockOrder {
		hs = append(hs, scoreLWClass(id, ss, span))
	}
	d := rank(FamilyLWLock, hs)
	d.Missing = missing
	d.Observed = lwObserved(ss, span)
	return d
}

// lwTally is one class's waits across the samples.
type lwTally struct {
	total, peak, peakActive, all int64
	hot                          int
	event                        string
	qid, qidWaits                int64
}

func tallyClass(id NodeID, ss []lwSample) lwTally {
	var t lwTally
	events, queries := map[string]int64{}, map[int64]int64{}
	for _, s := range ss {
		n := s.class[id]
		t.total += n
		for e, c := range s.events {
			t.all += c
			if cls, ok := LWLockClass(e); ok && cls == id {
				events[e] += c
			}
		}
		for q, c := range s.query[id] {
			queries[q] += c
		}
		if n >= lwMinWaiters {
			t.hot++
		}
		if n > t.peak {
			t.peak, t.peakActive = n, s.active
		}
	}
	t.event, _ = topKey(events)
	t.qid, t.qidWaits = topInt(queries)
	return t
}

func scoreLWClass(id NodeID, ss []lwSample, span float64) Hypothesis {
	h := newHypothesis(id, "LWLock waits")
	if len(ss) < lwMinSamples {
		return h
	}
	last := ss[len(ss)-1].ev
	t := tallyClass(id, ss)
	if t.total == 0 {
		h.contradict(last, fmt.Sprintf("no backend waited on its LWLocks in %d samples "+
			"over %s s", len(ss), fv(span)))
		return h
	}
	h.Subject = "LWLock " + probes.FormatValue(t.event)
	if t.hot < (len(ss)+1)/2 {
		return h
	}
	h.add(0.4, last, fmt.Sprintf("at least %d backends waited on LWLock %s in %d of %d "+
		"samples (peak %d)", lwMinWaiters, probes.FormatValue(t.event), t.hot, len(ss), t.peak))
	if t.total*2 >= t.all {
		h.add(0.2, last, fmt.Sprintf("it accounts for %s%% of the LWLock waits sampled",
			pct(float64(t.total)/float64(t.all))))
	}
	if t.peakActive > 0 && t.peak*lwActiveShare >= t.peakActive {
		h.add(0.1, last, fmt.Sprintf("the peak is %d of %d active backends", t.peak,
			t.peakActive))
	}
	if t.qidWaits > 0 && t.qidWaits*2 >= t.total {
		h.Subject = fmt.Sprintf("queryid %d", t.qid)
		h.add(0.1, last, fmt.Sprintf("queryid %d accounts for %d of the %d waits", t.qid,
			t.qidWaits, t.total))
	}
	return h
}

// lwObserved summarizes the samples: peak activity and the most waited
// LWLock events, modeled or not.
func lwObserved(ss []lwSample, span float64) []Fact {
	if len(ss) == 0 {
		return nil
	}
	var peak, waiting int64
	events := map[string]int64{}
	for _, s := range ss {
		peak = max(peak, s.active)
		var w int64
		for e, c := range s.events {
			events[e] += c
			w += c
		}
		waiting = max(waiting, w)
	}
	text := fmt.Sprintf("%d samples over %s s: at most %d active backends, at most %d "+
		"waiting on LWLocks", len(ss), fv(span), peak, waiting)
	if top := topEvents(events, 3); top != "" {
		text += "; most waited: " + top
	}
	return []Fact{{EvidenceID: ss[len(ss)-1].ev, Text: text}}
}

func topEvents(events map[string]int64, n int) string {
	type kv struct {
		k string
		v int64
	}
	var all []kv
	for k, v := range events {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].v > all[j].v || (all[i].v == all[j].v && all[i].k < all[j].k)
	})
	var parts []string
	for i, e := range all {
		if i == n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", probes.FormatValue(e.k), e.v))
	}
	return strings.Join(parts, ", ")
}

// topKey is the key with the largest count (ties to the lower key).
func topKey(m map[string]int64) (string, int64) {
	best, bestN := "", int64(-1)
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best, bestN
}

// topInt is the integer key with the largest count (ties to the lower).
func topInt(m map[int64]int64) (int64, int64) {
	var best, bestN int64
	found := false
	for k, n := range m {
		if !found || n > bestN || (n == bestN && k < best) {
			best, bestN, found = k, n, true
		}
	}
	return best, bestN
}
