package causal

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sequence exhaustion: a consuming sequence near the limit it reaches
// first. The binding limit (its own type, a narrower owning column or an
// explicit MAXVALUE) decides which mechanism holds, and the fix; the
// other two are ruled out. A cycling sequence wraps by design and a
// dormant one does not run out.

// SequenceNearHorizon is the measured runway to the limit that counts as
// near when less than half the range is used (product default).
const SequenceNearHorizon = 30 * 24 * time.Hour

var limitNodes = map[string]NodeID{probes.LimitSequenceType: SequenceTypeLimit,
	probes.LimitColumnType:  ColumnNarrowerThanSequence,
	probes.LimitExplicitMax: ExplicitMaxvalueLimit}

// seqEvidence is what the sequence family reads about one sequence.
type seqEvidence struct {
	last        probes.SequenceRunway
	ev          string
	delta       float64
	deltaKnown  bool
	span        float64
	trend       probes.RunwayTrend
	trendEv     string
	trendOK     bool
	consuming   bool
	flat        bool
	near        bool
	nearFact    Fact
	consumeFact Fact
}

// DiagnoseSequence scores the sequence named by subject ("sequence
// schema.name").
func DiagnoseSequence(obs []Observation, subject string) Diagnosis {
	name := strings.TrimPrefix(subject, "sequence ")
	samples, missing := walSeries(obs, probes.SequenceRunwayProbe)
	missing = append(missing, missingFor(obs, probes.RunwayTrendsProbe)...)
	d := Diagnosis{Family: FamilySequence, GraphVersion: GraphVersion, Subject: subject,
		Missing: missing}
	if samples.n == 0 {
		d.Reason = "sequence evidence unavailable"
		return d
	}
	last, ok := sequenceIn(samples.last, name)
	switch {
	case !ok:
		d.Reason = fmt.Sprintf("%s is not among the sequences nearest their limit", subject)
		return d
	case !probes.Known(last.LastValue):
		d.Missing = append(d.Missing, Missing{ProbeID: probes.SequenceRunwayProbe,
			Reason: "last_value_unreadable"})
		d.Reason = "the sequence's last value is unreadable"
		return d
	}
	e := readSequence(obs, samples, last)
	hs := []Hypothesis{scoreLimit(e, probes.LimitSequenceType),
		scoreLimit(e, probes.LimitColumnType), scoreLimit(e, probes.LimitExplicitMax)}
	ranked := rank(FamilySequence, hs)
	ranked.Missing, ranked.Observed = missing, e.observed()
	if ranked.Subject == "" {
		ranked.Subject = subject
	}
	return ranked
}

func sequenceIn(o Observation, name string) (probes.SequenceRunway, bool) {
	ss, err := probes.Sequences(o.Result)
	if err != nil {
		return probes.SequenceRunway{}, false
	}
	for _, s := range ss {
		if s.Sequence == name {
			return s, true
		}
	}
	return probes.SequenceRunway{}, false
}

func readSequence(obs []Observation, samples walEvidence,
	last probes.SequenceRunway) seqEvidence {
	e := seqEvidence{last: last, ev: samples.last.EvidenceID}
	if first, ok := sequenceIn(samples.first, last.Sequence); ok && samples.n > 1 &&
		probes.Known(first.LastValue) {
		e.delta, e.deltaKnown = last.LastValue-first.LastValue, true
		e.span = samples.last.Result.ObservedAt.Sub(samples.first.Result.ObservedAt).Seconds()
		e.consumeFact = Fact{EvidenceID: e.ev, Text: fmt.Sprintf("the sequence advanced "+
			"from %s to %s in %s s", probes.FormatValue(first.LastValue),
			probes.FormatValue(last.LastValue), probes.FormatValue(e.span))}
	}
	if o, ok := find(obs, probes.RunwayTrendsProbe); ok {
		ts, err := probes.RunwayTrends(o.Result)
		tr, found := probes.FindTrend(ts, probes.RunwaySequence, last.Sequence)
		e.trendOK = err == nil && found && tr.Samples >= minTrendSamples &&
			probes.Known(tr.RatePerS)
		e.trend, e.trendEv = tr, o.EvidenceID
	}
	e.classify()
	return e
}

// classify decides consumption and nearness.
func (e *seqEvidence) classify() {
	advanced := e.deltaKnown && e.delta > 0
	trendRising := e.trendOK && e.trend.RatePerS > 0
	e.consuming = advanced || trendRising
	e.flat = !advanced && e.trendOK && e.trend.RatePerS <= 0
	if !advanced && trendRising {
		e.consumeFact = Fact{EvidenceID: e.trendEv, Text: fmt.Sprintf("the sequence "+
			"advanced at %s values per second over %s s (%d samples)",
			probes.FormatValue(e.trend.RatePerS), probes.FormatValue(e.trend.SpanS()),
			e.trend.Samples)}
	}
	switch {
	case e.last.Fraction >= NearFraction:
		e.near = true
		e.nearFact = Fact{EvidenceID: e.ev, Text: fractionText(e.last)}
	case e.trendOK && e.trend.SecondsToLimit() <= SequenceNearHorizon.Seconds():
		e.near = true
		e.nearFact = Fact{EvidenceID: e.trendEv, Text: seqTrendText(e.trend)}
	}
}

func scoreLimit(e seqEvidence, limit string) Hypothesis {
	h := newHypothesis(limitNodes[limit], "sequence "+e.last.Sequence)
	binding := e.last.BindingLimit()
	switch {
	case limit != binding:
		h.contradict(e.ev, fmt.Sprintf("the binding limit is %s (%s), not %s",
			limitText(binding, e.last), probes.FormatValue(e.last.Limit),
			limitText(limit, e.last)))
	case e.last.Cycle:
		h.contradict(e.ev, fmt.Sprintf("%s has CYCLE: it wraps to its minimum at the "+
			"limit instead of failing", e.last.Sequence))
	case e.flat:
		h.contradict(e.trendEv, fmt.Sprintf("%s did not advance between the samples and "+
			"its trend over %s s is flat", e.last.Sequence,
			probes.FormatValue(e.trend.SpanS())))
	default:
		if e.consuming {
			h.add(mechWeight, e.consumeFact.EvidenceID, e.consumeFact.Text)
		}
		if e.near {
			h.add(nearWeight, e.nearFact.EvidenceID, e.nearFact.Text)
		}
	}
	return h
}

func limitText(limit string, s probes.SequenceRunway) string {
	switch limit {
	case probes.LimitColumnType:
		return fmt.Sprintf("the owning column %s's type %s", s.OwnerColumn, s.OwnerType)
	case probes.LimitExplicitMax:
		return "the sequence's explicit MAXVALUE"
	default:
		return fmt.Sprintf("the sequence's type %s", s.DataType)
	}
}

func fractionText(s probes.SequenceRunway) string {
	return fmt.Sprintf("%s is at %s of its limit %s (%s); its last value is %s",
		s.Sequence, probes.FormatValue(s.Fraction), probes.FormatValue(s.Limit),
		limitText(s.BindingLimit(), s), probes.FormatValue(s.LastValue))
}

func seqTrendText(tr probes.RunwayTrend) string {
	return fmt.Sprintf("at %s values per second (%d samples over %s s) %s reaches its "+
		"limit %s in %s s", probes.FormatValue(tr.RatePerS), tr.Samples,
		probes.FormatValue(tr.SpanS()), tr.Subject, probes.FormatValue(tr.Limit),
		probes.FormatValue(tr.SecondsToLimit()))
}

func (e seqEvidence) observed() []Fact {
	out := []Fact{{EvidenceID: e.ev, Text: fractionText(e.last)}}
	if s := e.trend.SecondsToLimit(); e.trendOK && !math.IsNaN(s) && !math.IsInf(s, 0) {
		out = append(out, Fact{EvidenceID: e.trendEv, Text: seqTrendText(e.trend)})
	}
	return out
}
