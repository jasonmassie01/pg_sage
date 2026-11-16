package outcomes

import "sync"

// pg_sage_outcomes_measured_ratio is the share of changes with a measured
// effect, per database, at the last read of the default window with no
// filter (the spec's "share of changes with a measured effect"). The
// process keeps it in memory; a window with no changes has no sample.

var measured = struct {
	sync.Mutex
	ratio map[string]float64
}{ratio: map[string]float64{}}

// Observe records database's measured share from r when q is the default,
// unfiltered window and the read was complete.
func Observe(database string, q Query, r Result) {
	if database == "" || q.Since != DefaultSince || q.Verdict != "" || q.Evidence != "" ||
		r.Truncated || len(r.Errors) > 0 {
		return
	}
	measured.Lock()
	defer measured.Unlock()
	if r.Summary.MeasuredShare == nil {
		delete(measured.ratio, database)
		return
	}
	measured.ratio[database] = *r.Summary.MeasuredShare
}

// MeasuredRatios is a copy of the recorded shares by database.
func MeasuredRatios() map[string]float64 {
	measured.Lock()
	defer measured.Unlock()
	out := make(map[string]float64, len(measured.ratio))
	for k, v := range measured.ratio {
		out[k] = v
	}
	return out
}

// resetMeasured forgets every recorded share (tests).
func resetMeasured() {
	measured.Lock()
	defer measured.Unlock()
	measured.ratio = map[string]float64{}
}
