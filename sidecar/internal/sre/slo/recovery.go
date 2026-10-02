package slo

import "time"

// Slice is one recovery observation: the SLI over [Start, End].
type Slice struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Bad      float64   `json:"bad"`
	Eligible float64   `json:"eligible"`
	Resets   int       `json:"resets"`
	Unknown  string    `json:"unknown,omitempty"`
}

// RecoveryState is the verdict of the SLI recovery predicate.
type RecoveryState string

// Recovery verdicts.
const (
	RecoveryRecovered    RecoveryState = "recovered"
	RecoveryNotRecovered RecoveryState = "not_recovered"
	RecoveryUnknown      RecoveryState = "unknown"
)

// DefaultMinSlices is how many consecutive fresh slices certify recovery.
const DefaultMinSlices = 3

// RecoveryParams tune the predicate. BaselineEligible is the eligible
// events per slice before the incident (zero when unknown): a slice
// under half of it means traffic dropped, which cannot certify recovery.
type RecoveryParams struct {
	Target           float64
	MinEligible      float64
	BaselineEligible float64
	MinSlices        int
}

// Recovery is the predicate's verdict over the newest slices.
type Recovery struct {
	State     RecoveryState `json:"state"`
	Reason    string        `json:"reason,omitempty"`
	Since     time.Time     `json:"since"`
	Evaluated int           `json:"evaluated"`
	Slices    []Slice       `json:"slices"`
}

// EvaluateRecovery certifies customer recovery only when each of the
// newest MinSlices slices has trustworthy data (no gap, no counter
// reset, enough traffic, traffic not collapsed) and burns under 1x. A
// clearly burning slice is "not_recovered" even next to unknown ones.
func EvaluateRecovery(slices []Slice, p RecoveryParams) Recovery {
	n := p.MinSlices
	if n <= 0 {
		n = DefaultMinSlices
	}
	r := Recovery{State: RecoveryUnknown, Slices: append([]Slice{}, slices...)}
	if len(slices) < n {
		r.Reason = firstUnknown(slices, p)
		if r.Reason == "" {
			r.Reason = ReasonInsufficientSamples
		}
		return r
	}
	newest := slices[len(slices)-n:]
	r.Evaluated = n
	for _, s := range newest {
		if sliceUnknown(s, p) == "" && burns(s, p.Target) {
			r.State, r.Reason = RecoveryNotRecovered, ReasonBurning
			return r
		}
	}
	if reason := firstUnknown(newest, p); reason != "" {
		r.Reason = reason
		return r
	}
	r.State = RecoveryRecovered
	return r
}

func firstUnknown(slices []Slice, p RecoveryParams) string {
	for _, s := range slices {
		if reason := sliceUnknown(s, p); reason != "" {
			return reason
		}
	}
	return ""
}

func sliceUnknown(s Slice, p RecoveryParams) string {
	switch {
	case s.Unknown != "":
		return s.Unknown
	case s.Resets > 0:
		return ReasonCounterReset
	case s.Eligible <= 0:
		return ReasonZeroEligible
	case s.Bad < 0 || s.Bad > s.Eligible:
		return ReasonInvalidValue
	case s.Eligible < p.MinEligible:
		return ReasonLowTraffic
	case p.BaselineEligible > 0 && s.Eligible < p.BaselineEligible/2:
		return ReasonTrafficDropped
	}
	return ""
}

// burnTolerance absorbs float rounding of 1 - target.
const burnTolerance = 1e-9

// burns reports a slice spending the budget at 1x or faster.
func burns(s Slice, target float64) bool {
	return (s.Bad/s.Eligible)/(1-target) >= 1-burnTolerance
}
