package runway

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// EvalInput is what one tick evaluates: the measured trends (and where
// their row cap cut them), the current XID reading (nil when unreadable:
// the wraparound trends are then held, not projected) and the current
// tables, slots and sequences, each with whether it could be read.
type EvalInput struct {
	XID             *probes.XIDRunway
	Trends          []probes.RunwayTrend
	TrendsOK        bool
	TrendsTruncated bool
	TrendsLastKind  string // the kind of the last trend a truncated read returned
	Tables          []probes.WraparoundTable
	TablesOK        bool
	Slots           []probes.Slot
	SlotsOK         bool
	// OpenSlots names the slots with an open finding, read only while
	// the slots are unreadable (such a slot may still exist).
	OpenSlots   []string
	Sequences   []probes.SequenceRunway
	SequencesOK bool
	// Capacity is this tick's disk capacity; a disk runway projected
	// against it names its source.
	Capacity DiskCapacity
}

// Runway is one runway that crossed its horizon: the forecast finding it
// opens and the pre-incident investigation it asks for.
type Runway struct {
	Category       string
	ObjectType     string
	Identifier     string
	Kind           sre.TriggerKind
	Subject        string
	Severity       string
	SecondsToLimit float64 // NaN for an overdue table (already past its maximum)
	Title          string
	Recommendation string
	Detail         map[string]any
}

// Evaluate returns the runways that crossed their horizons and the
// categories it could evaluate completely (only those may resolve their
// cleared findings). A category whose trends a truncated read may have
// cut is not evaluated; the runways of the rows returned still open.
func Evaluate(in EvalInput, opts Options) ([]Runway, []string) {
	var out []Runway
	var evaluated []string
	if in.TrendsOK && in.TablesOK && in.trendsComplete(CategoryWraparound) {
		evaluated = append(evaluated, CategoryWraparound)
	}
	if in.TrendsOK && in.trendsComplete(CategoryWAL) {
		evaluated = append(evaluated, CategoryWAL)
	}
	if in.TrendsOK && in.SequencesOK && in.trendsComplete(CategorySequence) {
		evaluated = append(evaluated, CategorySequence)
	}
	if in.TrendsOK {
		for _, tr := range in.Trends {
			if r, ok := trendRunway(tr, in, opts); ok {
				out = append(out, r)
			}
		}
	}
	if in.TablesOK {
		out = append(out, overdueTables(in.Tables, opts)...)
	}
	return out, evaluated
}

// measured reports a trend read from enough samples over enough time.
func measured(tr probes.RunwayTrend, opts Options) bool {
	return tr.Samples >= int64(opts.MinSamples) && tr.SpanS() >= opts.MinSpan.Seconds() &&
		probes.Known(tr.RatePerS)
}

// trendRunway projects one series against its family's horizons.
func trendRunway(tr probes.RunwayTrend, in EvalInput, opts Options) (Runway, bool) {
	if !measured(tr, opts) {
		return Runway{}, false
	}
	var r Runway
	var horizon, critical time.Duration
	switch tr.Kind {
	case probes.RunwayXID, probes.RunwayMXID:
		return ageRunway(tr, in, opts)
	case probes.RunwayDiskUsed, probes.RunwayWALSlot:
		if !(tr.R2 >= levelMinR2) {
			return Runway{}, false
		}
		r = walRunway(tr)
		horizon, critical = opts.DiskHorizon, opts.DiskCritical
	case probes.RunwaySequence:
		if sq, _ := sequenceOf(in.Sequences, tr.Subject); sq.Cycle {
			return Runway{}, false
		}
		r = Runway{Category: CategorySequence, ObjectType: "sequence",
			Identifier: tr.Subject, Kind: sre.TriggerSequence,
			Subject: "sequence " + tr.Subject}
		horizon, critical = opts.SequenceHorizon, opts.SequenceCritical
	default:
		return Runway{}, false
	}
	if !project(&r, tr, horizon, critical) {
		return Runway{}, false
	}
	r.Detail = trendDetail(tr, horizon)
	source := diskSource(tr, in.Capacity)
	if source != "" {
		r.Detail["capacity_source"] = source
	}
	r.Title, r.Recommendation = trendWords(r, tr, source)
	if sq, ok := sequenceOf(in.Sequences, tr.Subject); ok && sq.Descending() {
		describeDescending(&r, tr)
	}
	return r, true
}

// project sets r's projection and severity when tr reaches its limit
// inside horizon (critical inside critical); false when it does not.
func project(r *Runway, tr probes.RunwayTrend, horizon, critical time.Duration) bool {
	secs := tr.SecondsToLimit()
	if math.IsNaN(secs) || !within(secs, horizon) {
		return false
	}
	r.SecondsToLimit, r.Severity = secs, SeverityWarning
	if within(secs, critical) {
		r.Severity = SeverityCritical
	}
	return true
}

func walRunway(tr probes.RunwayTrend) Runway {
	if tr.Kind == probes.RunwayDiskUsed {
		return Runway{Category: CategoryWAL, ObjectType: "database", Identifier: "disk",
			Kind: sre.TriggerDiskWAL, Subject: "disk"}
	}
	return Runway{Category: CategoryWAL, ObjectType: "replication_slot",
		Identifier: "slot:" + tr.Subject, Kind: sre.TriggerDiskWAL,
		Subject: "slot " + tr.Subject}
}

// sequenceOf returns the sequence name of the current reading.
func sequenceOf(ss []probes.SequenceRunway, name string) (probes.SequenceRunway, bool) {
	for _, s := range ss {
		if s.Sequence == name {
			return s, true
		}
	}
	return probes.SequenceRunway{}, false
}

// describeDescending shows a descending sequence's own values (its series
// is sampled negated, see sequenceSeries) and says that it falls.
func describeDescending(r *Runway, tr probes.RunwayTrend) {
	r.Detail["limit"], r.Detail["last_value"] = -tr.Limit, -tr.LastValue
	r.Detail["direction"] = "descending"
	r.Recommendation = fmt.Sprintf("Sequence %s falls at %s per second (measured over "+
		"%d samples). A read-only pre-incident investigation explains what drives it; "+
		"act before its binding limit is reached.", tr.Subject,
		probes.FormatValue(round2(tr.RatePerS)), tr.Samples)
}

func trendDetail(tr probes.RunwayTrend, horizon time.Duration) map[string]any {
	return map[string]any{"runway_kind": tr.Kind, "subject": tr.Subject,
		"seconds_to_limit": round2(tr.SecondsToLimit()), "rate_per_s": round2(tr.RatePerS),
		"limit": tr.Limit, "last_value": tr.LastValue, "samples": tr.Samples,
		"span_s": round2(tr.SpanS()), "r2": finiteOrNil(tr.R2),
		"horizon_s": horizon.Seconds()}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func finiteOrNil(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return round2(v)
}

// diskSource is the capacity source of a disk runway whose limit is this
// tick's capacity; a limit sampled under another capacity names none.
func diskSource(tr probes.RunwayTrend, c DiskCapacity) string {
	if tr.Kind != probes.RunwayDiskUsed || c.Source == "" || tr.Limit != c.Bytes {
		return ""
	}
	return c.Source
}

// trendWords titles a disk, slot or sequence runway (ageWords titles the
// wraparound ones); source names a provider-measured disk capacity.
func trendWords(r Runway, tr probes.RunwayTrend, source string) (string, string) {
	when := (time.Duration(r.SecondsToLimit) * time.Second).Round(time.Minute)
	what := map[string]string{probes.RunwayDiskUsed: "Disk usage",
		probes.RunwayWALSlot:  fmt.Sprintf("WAL retained by slot %s", tr.Subject),
		probes.RunwaySequence: fmt.Sprintf("Sequence %s", tr.Subject)}[tr.Kind]
	limit := map[string]string{probes.RunwayDiskUsed: "the declared disk capacity",
		probes.RunwayWALSlot:  "its configured WAL maximum",
		probes.RunwaySequence: "its binding limit"}[tr.Kind]
	if source == CapacityProviderTelemetry {
		limit = "the provider-reported disk capacity"
	}
	title := fmt.Sprintf("%s reaches %s in about %s", what, limit, when)
	rec := fmt.Sprintf("%s grows at %s per second (measured over %d samples). A "+
		"read-only pre-incident investigation explains what drives it; act before "+
		"%s is reached.", what, probes.FormatValue(round2(tr.RatePerS)), tr.Samples, limit)
	return title, rec
}

// overdueTables are tables this far past their effective freeze maximum.
func overdueTables(ts []probes.WraparoundTable, opts Options) []Runway {
	var out []Runway
	for _, t := range ts {
		f := t.Fraction()
		if !(f >= OverdueRatio) {
			continue
		}
		sev := SeverityWarning
		if f >= OverdueCriticalRatio {
			sev = SeverityCritical
		}
		out = append(out, Runway{Category: CategoryWraparound, ObjectType: "table",
			Identifier: t.Relation, Kind: sre.TriggerWraparound,
			Subject: "table " + t.Relation, Severity: sev, SecondsToLimit: math.NaN(),
			Title: fmt.Sprintf("Table %s is overdue for freezing (%s times its freeze "+
				"maximum)", t.Relation, probes.FormatValue(round2(f))),
			Recommendation: "Autovacuum has not frozen this table past its maximum. A " +
				"read-only pre-incident investigation looks for what holds it back.",
			Detail: map[string]any{"runway_kind": "overdue_table", "relation": t.Relation,
				"xid_age":        finiteOrNil(t.XIDAge),
				"freeze_max_age": finiteOrNil(t.FreezeMaxAge), "mxid_age": finiteOrNil(t.MXIDAge),
				"mxid_freeze_max_age": finiteOrNil(t.MXIDFreezeMaxAge),
				"fraction":            round2(f), "overdue_ratio": OverdueRatio}})
	}
	return out
}

// Finding is the forecast finding a runway opens. It carries no SQL:
// nothing acts on a forecast directly.
func (r Runway) Finding() analyzer.Finding {
	return analyzer.Finding{Category: r.Category, Severity: r.Severity,
		ObjectType: r.ObjectType, ObjectIdentifier: r.Identifier,
		Title: strings.TrimSpace(r.Title), Detail: r.Detail,
		Recommendation: r.Recommendation, ActionRisk: "safe"}
}

// within reports a projection inside a horizon, allowing for the float
// error of a projection computed to land exactly on it.
func within(secs float64, horizon time.Duration) bool {
	return secs <= horizon.Seconds()+1e-6
}
