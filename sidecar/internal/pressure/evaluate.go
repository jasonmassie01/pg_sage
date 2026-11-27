package pressure

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Evaluation is everything one answer is computed from.
type Evaluation struct {
	// Readings are the inputs by name; a missing one is unknown.
	Readings map[string]Reading
	Limits   Limits
	// XIDFreezeMax and MXIDFreezeMax are the server's freeze maxima (0:
	// unread), the default slow bounds of the two ID ages.
	XIDFreezeMax  float64
	MXIDFreezeMax float64
	// Batch is the caller's batch size (0: none).
	Batch int
	// CatchUp is how fast replica lag is falling, in seconds of lag per
	// second (negative: growing); CatchUpKnown says it was measured.
	CatchUp      float64
	CatchUpKnown bool
	// MinSleep and MaxSleep bound SleepSeconds (defaults 2 s and 60 s).
	MinSleep time.Duration
	MaxSleep time.Duration
	Now      time.Time
}

const (
	defaultMinSleep = 2 * time.Second
	defaultMaxSleep = time.Minute
)

// Evaluate is the answer: stop when any known input is at or past its
// stop bound, when a required input is unknown, or when no input is known
// at all; else slow when any is in its slow band, halving the batch for
// each; else proceed. An unknown optional input is reported and never
// decides. The order of Inputs is policy.PressureInputs.
func Evaluate(e Evaluation) Verdict {
	v := Verdict{Verdict: Proceed, Reasons: []string{}}
	slow, known := 0, 0
	for _, name := range policy.PressureInputs() {
		in := e.input(name)
		v.Inputs = append(v.Inputs, in)
		switch {
		case in.State == StateKnown:
			known++
			slow += v.band(in)
		case in.State == StateUnknown && in.Required:
			v.Verdict = Stop
			v.Reasons = append(v.Reasons, fmt.Sprintf("%s is unknown (%s) and required, "+
				"so the answer stops", name, in.Reason))
		}
	}
	if known == 0 {
		v.Verdict = Stop
		v.Reasons = append(v.Reasons, "no input could be read, so the answer stops")
	}
	if v.Verdict != Stop && slow > 0 {
		v.Verdict = Slow
	}
	v.BatchFactor = factor(v.Verdict, slow)
	v.SuggestedBatch = suggestBatch(v.Verdict, v.BatchFactor, e.Batch)
	v.SleepSeconds = e.sleep(v.Verdict, v.Inputs)
	return v
}

// band records a known input's band in v and returns 1 when it slows.
func (v *Verdict) band(in Input) int {
	switch in.Band {
	case BandStop:
		v.Verdict = Stop
		v.Reasons = append(v.Reasons, fmt.Sprintf("%s %s %s is at or past its stop limit %s",
			in.Name, number(*in.Value), in.Unit, number(*in.StopLimit)))
	case BandSlow:
		v.Reasons = append(v.Reasons, fmt.Sprintf("%s %s %s is at or past its slow limit %s",
			in.Name, number(*in.Value), in.Unit, number(*in.SlowLimit)))
		return 1
	}
	return 0
}

func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func factor(k Kind, slow int) float64 {
	switch k {
	case Stop:
		return 0
	case Slow:
		return math.Pow(0.5, float64(slow))
	}
	return 1
}

// suggestBatch is batch times factor: the batch itself to proceed, at
// least 1 to slow, 0 to stop or when no batch was given.
func suggestBatch(k Kind, factor float64, batch int) int {
	switch {
	case batch <= 0 || k == Stop:
		return 0
	case k == Slow:
		return max(1, int(math.Floor(float64(batch)*factor)))
	}
	return batch
}

// input is name's reading against its limits.
func (e Evaluation) input(name string) Input {
	r, ok := e.Readings[name]
	if !ok {
		r = Unknown("not read")
	}
	if r.State == StateKnown && (math.IsNaN(r.Value) || math.IsInf(r.Value, 0)) {
		r = Unknown("the value read was not a finite number")
	}
	slow, stop := e.bounds(name)
	in := Input{Name: name, State: r.State, Unit: units[name], SlowLimit: slow,
		StopLimit: stop, Required: e.Limits.Required[name], Reason: r.Reason}
	if r.State != StateKnown {
		return in
	}
	value, at := r.Value, r.At
	age := math.Max(0, e.Now.Sub(at).Seconds())
	in.Value, in.ObservedAt, in.AgeSeconds = &value, &at, &age
	in.Band = bandOf(name, value, slow, stop)
	return in
}

// bounds are name's effective slow and stop bounds.
func (e Evaluation) bounds(name string) (*float64, *float64) {
	l := e.Limits.Inputs[name]
	if !l.SlowAtFreezeMax {
		return l.Slow, l.Stop
	}
	freezeMax := e.XIDFreezeMax
	if name == policy.PressureMultixactAge {
		freezeMax = e.MXIDFreezeMax
	}
	switch {
	case l.Stop == nil && freezeMax > 0:
		return ptr(freezeMax), nil
	case l.Stop == nil:
		return nil, nil
	case freezeMax > 0 && freezeMax < *l.Stop:
		return ptr(freezeMax), l.Stop
	}
	return ptr(*l.Stop / 2), l.Stop
}

func bandOf(name string, v float64, slow, stop *float64) Band {
	past := func(limit *float64) bool {
		if limit == nil {
			return false
		}
		if policy.PressureLowerIsWorse(name) {
			return v <= *limit
		}
		return v >= *limit
	}
	switch {
	case past(stop):
		return BandStop
	case past(slow):
		return BandSlow
	}
	return BandOK
}

// sleep is how long to wait: none to proceed; while replica lag is in its
// slow or stop band, the time for it to fall under the slow bound at the
// measured catch-up rate (the excess itself when unmeasured, the maximum
// when lag is not falling); otherwise the minimum.
func (e Evaluation) sleep(k Kind, inputs []Input) float64 {
	if k == Proceed {
		return 0
	}
	lo, hi := e.MinSleep, e.MaxSleep
	if lo <= 0 {
		lo = defaultMinSleep
	}
	if hi <= 0 {
		hi = defaultMaxSleep
	}
	hi = max(hi, lo)
	for _, in := range inputs {
		if in.Name != policy.PressureReplicaLag || in.State != StateKnown ||
			in.Band == BandOK {
			continue
		}
		excess := *in.Value
		if in.SlowLimit != nil {
			excess -= *in.SlowLimit
		}
		s := hi.Seconds()
		switch {
		case !e.CatchUpKnown:
			s = excess
		case e.CatchUp > 0:
			s = excess / e.CatchUp
		}
		return math.Min(hi.Seconds(), math.Max(lo.Seconds(), s))
	}
	return lo.Seconds()
}
