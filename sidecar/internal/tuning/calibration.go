package tuning

import (
	"math"
	"sort"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Calibrated confidence (owner decision 4): for each action class and
// prediction method, what the outcome ledger observed for comparable
// predictions. Below the minimum number of decided outcomes the answer is
// "uncalibrated": a confidence is never invented.

// DefaultMinOutcomes is the default number of decided outcomes a class
// and method need before a confidence is given.
const DefaultMinOutcomes = 5

// wilsonZ is the 95% normal quantile of the Wilson score interval.
const wilsonZ = 1.96

// Calibration statuses and bases.
const (
	StatusCalibrated   = "calibrated"
	StatusUncalibrated = "uncalibrated"
	BasisBin           = "bin"
	BasisClassMethod   = "class_method"
)

// binEdges are the predicted-change bins, by magnitude in percent.
var binEdges = []struct {
	label     string
	low, high float64
}{{"0-10", 0, 10}, {"10-25", 10, 25}, {"25-50", 25, 50}, {"50+", 50, math.Inf(1)}}

// OutcomeSample is one decided (or not yet decided) outcome.
type OutcomeSample struct {
	Class        string
	Method       string
	PredictedPct float64
	Verdict      string
	Tolerance    string
	ObservedPct  *float64
}

// Bin is one reliability bin. Predicted and observed means are
// improvements: a change in the predicted direction is positive.
type Bin struct {
	Label         string  `json:"label"`
	Low           float64 `json:"low"`
	High          float64 `json:"-"`
	N             int     `json:"n"`
	Hits          int     `json:"hits"`
	Regressed     int     `json:"regressed"`
	ToleranceMet  int     `json:"tolerance_met"`
	Rate          float64 `json:"rate"`
	WilsonLow     float64 `json:"wilson_low"`
	WilsonHigh    float64 `json:"wilson_high"`
	MeanPredicted float64 `json:"mean_predicted"`
	MeanObserved  float64 `json:"mean_observed"`
	observed      int
}

// ClassCalibration is one class and method's record.
type ClassCalibration struct {
	Class      string  `json:"class"`
	Method     string  `json:"method"`
	N          int     `json:"n"`
	Hits       int     `json:"hits"`
	Regressed  int     `json:"regressed"`
	Rate       float64 `json:"rate"`
	WilsonLow  float64 `json:"wilson_low"`
	WilsonHigh float64 `json:"wilson_high"`
	Bins       []Bin   `json:"bins"`
	Status     string  `json:"status"`
}

// Calibration is the ledger's record per class and method.
type Calibration struct {
	MinOutcomes int                `json:"min_outcomes"`
	Classes     []ClassCalibration `json:"classes"`
	Excluded    int                `json:"excluded"`
}

// Confidence is the calibrated confidence of one prediction.
type Confidence struct {
	Status     string
	Value      *float64
	Basis      string
	N          int
	Hits       int
	Bin        string
	WilsonLow  float64
	WilsonHigh float64
}

// decided are the verdicts that judge a prediction; improved is a hit.
var decided = map[string]bool{verify.OutcomeImproved: true, verify.OutcomeNeutral: true,
	verify.OutcomeRegressed: true}

// Calibrate builds the calibration from samples; minOutcomes below 1
// means DefaultMinOutcomes. Undecided samples and samples without a
// prediction are excluded.
func Calibrate(samples []OutcomeSample, minOutcomes int) Calibration {
	if minOutcomes < 1 {
		minOutcomes = DefaultMinOutcomes
	}
	cal := Calibration{MinOutcomes: minOutcomes, Classes: []ClassCalibration{}}
	byKey := map[[2]string]*ClassCalibration{}
	for _, s := range samples {
		if !decided[s.Verdict] || s.Method == verify.MethodNone || s.Method == "" {
			cal.Excluded++
			continue
		}
		k := [2]string{s.Class, s.Method}
		cc := byKey[k]
		if cc == nil {
			cc = &ClassCalibration{Class: s.Class, Method: s.Method, Bins: emptyBins()}
			byKey[k] = cc
		}
		addSample(cc, s)
	}
	for _, cc := range byKey {
		finish(cc, minOutcomes)
		cal.Classes = append(cal.Classes, *cc)
	}
	sort.Slice(cal.Classes, func(i, j int) bool {
		a, b := cal.Classes[i], cal.Classes[j]
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Method < b.Method
	})
	return cal
}

func emptyBins() []Bin {
	out := make([]Bin, len(binEdges))
	for i, e := range binEdges {
		out[i] = Bin{Label: e.label, Low: e.low, High: e.high}
	}
	return out
}

func addSample(cc *ClassCalibration, s OutcomeSample) {
	b := &cc.Bins[binIndex(s.PredictedPct)]
	cc.N++
	b.N++
	switch s.Verdict {
	case verify.OutcomeImproved:
		cc.Hits++
		b.Hits++
	case verify.OutcomeRegressed:
		cc.Regressed++
		b.Regressed++
	}
	if s.Tolerance == verify.ToleranceMet {
		b.ToleranceMet++
	}
	sign := 1.0
	if s.PredictedPct <= 0 {
		sign = -1
	}
	b.MeanPredicted += math.Abs(s.PredictedPct)
	if s.ObservedPct != nil {
		b.MeanObserved += sign * *s.ObservedPct
		b.observed++
	}
}

func finish(cc *ClassCalibration, minOutcomes int) {
	cc.Rate = rate(cc.Hits, cc.N)
	cc.WilsonLow, cc.WilsonHigh = wilson(cc.Hits, cc.N)
	cc.Status = StatusUncalibrated
	if cc.N >= minOutcomes {
		cc.Status = StatusCalibrated
	}
	for i := range cc.Bins {
		b := &cc.Bins[i]
		b.Rate = rate(b.Hits, b.N)
		b.WilsonLow, b.WilsonHigh = wilson(b.Hits, b.N)
		if b.N > 0 {
			b.MeanPredicted /= float64(b.N)
		}
		if b.observed > 0 {
			b.MeanObserved /= float64(b.observed)
		}
	}
}

func rate(k, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(k) / float64(n)
}

// binIndex is the bin of a predicted change, by magnitude.
func binIndex(pct float64) int {
	m := math.Abs(pct)
	for i, e := range binEdges {
		if m < e.high {
			return i
		}
	}
	return len(binEdges) - 1
}

func binLabel(pct float64) string { return binEdges[binIndex(pct)].label }

// ConfidenceFor is the confidence of a prediction of class and method of
// predictedPct: its bin when the bin has enough outcomes, else the class
// and method's pool when that has enough, else uncalibrated.
func (c Calibration) ConfidenceFor(class, method string, predictedPct float64) Confidence {
	need := c.MinOutcomes
	if need < 1 {
		need = DefaultMinOutcomes
	}
	label := binLabel(predictedPct)
	for _, cc := range c.Classes {
		if cc.Class != class || cc.Method != method {
			continue
		}
		b := cc.Bins[binIndex(predictedPct)]
		if b.N >= need {
			return calibrated(BasisBin, label, b.Hits, b.N, b.WilsonLow, b.WilsonHigh)
		}
		if cc.N >= need {
			return calibrated(BasisClassMethod, label, cc.Hits, cc.N, cc.WilsonLow,
				cc.WilsonHigh)
		}
		return Confidence{Status: StatusUncalibrated, N: cc.N, Hits: cc.Hits, Bin: label}
	}
	return Confidence{Status: StatusUncalibrated, Bin: label}
}

func calibrated(basis, label string, hits, n int, lo, hi float64) Confidence {
	v := rate(hits, n)
	return Confidence{Status: StatusCalibrated, Value: &v, Basis: basis, N: n, Hits: hits,
		Bin: label, WilsonLow: lo, WilsonHigh: hi}
}

// wilson is the 95% Wilson score interval of k successes in n trials
// (zeros for no trials).
func wilson(k, n int) (float64, float64) {
	if n <= 0 {
		return 0, 0
	}
	p, nf, z2 := float64(k)/float64(n), float64(n), wilsonZ*wilsonZ
	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	margin := wilsonZ * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf)) / denom
	lo, hi := math.Max(center-margin, 0), math.Min(center+margin, 1)
	if k == 0 {
		lo = 0
	}
	if k == n {
		hi = 1
	}
	return lo, hi
}
