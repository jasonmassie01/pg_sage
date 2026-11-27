package perfgate

import (
	"sort"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// MeterLog collects the product meter's readings and the collector's
// snapshot times over a run, each once (by its time). The runtime test
// samples them from a ticker while the phases are read: it is safe for
// concurrent use.
type MeterLog struct {
	mu        sync.Mutex
	costs     []selfcost.Cost
	seenCost  map[int64]bool
	snapshots []time.Time
	seenSnap  map[int64]bool
}

// NewMeterLog returns an empty log.
func NewMeterLog() *MeterLog {
	return &MeterLog{seenCost: map[int64]bool{}, seenSnap: map[int64]bool{}}
}

// AddCost records a reading not seen before; a zero reading (the meter
// has not measured yet) is not recorded.
func (l *MeterLog) AddCost(c selfcost.Cost) bool {
	if l == nil || c.At.IsZero() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seenCost[c.At.UnixNano()] {
		return false
	}
	l.seenCost[c.At.UnixNano()] = true
	l.costs = append(l.costs, c)
	return true
}

// AddSnapshot records a collector snapshot time not seen before.
func (l *MeterLog) AddSnapshot(at time.Time) bool {
	if l == nil || at.IsZero() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seenSnap[at.UnixNano()] {
		return false
	}
	l.seenSnap[at.UnixNano()] = true
	l.snapshots = append(l.snapshots, at)
	return true
}

// Costs is the readings whose window lies within [from, to], oldest
// first: a reading whose window started before the phase also measured
// the phase before it.
func (l *MeterLog) Costs(from, to time.Time) []selfcost.Cost {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []selfcost.Cost{}
	for _, c := range l.costs {
		start := c.At.Add(-time.Duration(c.WindowSeconds * float64(time.Second)))
		if !start.Before(from) && !c.At.After(to) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// Period is the collector period observed in [from, to]: the median gap
// between consecutive snapshots, 0 (unknown) with fewer than two.
func (l *MeterLog) Period(from, to time.Time) time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	var in []time.Time
	for _, at := range l.snapshots {
		if !at.Before(from) && !at.After(to) {
			in = append(in, at)
		}
	}
	l.mu.Unlock()
	if len(in) < 2 {
		return 0
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Before(in[j]) })
	gaps := make([]time.Duration, 0, len(in)-1)
	for i := 1; i < len(in); i++ {
		gaps = append(gaps, in[i].Sub(in[i-1]))
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return gaps[(len(gaps)-1)/2]
}
