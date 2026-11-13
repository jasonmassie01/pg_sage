package analyzer

import "time"

// anomalyState is one series' run of anomalous and normal cycles.
type anomalyState struct {
	streak  int // consecutive anomalous cycles
	calm    int // consecutive normal cycles since the finding opened
	open    bool
	episode time.Time // the first anomalous cycle of the episode
}

// anomalyTracker remembers, per finding identity, the cycles that open
// and resolve a workload_anomaly: two consecutive anomalous cycles open
// it, two consecutive normal ones resolve it. Only touched by the cycle
// goroutine; it starts empty after a restart.
type anomalyTracker map[string]*anomalyState

// Anomaly hysteresis: cycles to open and to resolve a finding.
const (
	anomalyOpenCycles    = 2
	anomalyResolveCycles = 2
)

// judged records a cycle with a verdict on ident. emit reports that the
// finding is (still) open with the given episode; keep that an open
// finding is kept open: on its first normal cycle, and on an anomalous
// cycle that does not open one yet (after a restart the tracker is empty
// while sage.findings still holds the open finding).
func (t anomalyTracker) judged(ident string, anomalous bool,
	at time.Time) (emit, keep bool, episode time.Time) {
	st := t[ident]
	if !anomalous {
		if st == nil || !st.open {
			delete(t, ident)
			return false, false, time.Time{}
		}
		st.streak, st.calm = 0, st.calm+1
		if st.calm < anomalyResolveCycles {
			return false, true, time.Time{}
		}
		delete(t, ident)
		return false, false, time.Time{}
	}
	if st == nil {
		st = &anomalyState{episode: at}
		t[ident] = st
	}
	st.streak, st.calm = st.streak+1, 0
	if st.open || st.streak >= anomalyOpenCycles {
		st.open = true
		return true, false, st.episode
	}
	return false, true, time.Time{}
}

// unjudged records a cycle without a verdict on ident (a learning bucket,
// an idle statement): an open finding is kept, a run in progress ends.
func (t anomalyTracker) unjudged(ident string) (keep bool) {
	st := t[ident]
	if st != nil && st.open {
		return true
	}
	delete(t, ident)
	return false
}
