package causal

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sequence exhaustion (AI-SRE-SPEC §4 R2): a consuming sequence near the
// limit it reaches first: its own type, a narrower owning column, or an
// explicit MAXVALUE. The binding limit decides the fix; a dormant or
// cycling sequence, or one far from its limit, is not an incident.

const (
	int4Max = 2147483647.0
	int8Max = 9223372036854775807.0
)

type seqRow struct {
	name, typ                     string
	last, max, typeMax, ownerMax  float64
	cycle                         bool
}

func (s seqRow) row() probes.Row {
	limit := s.max
	if !math.IsNaN(s.ownerMax) && s.ownerMax < limit {
		limit = s.ownerMax
	}
	r := probes.Row{"sequence": s.name, "data_type": s.typ, "increment_by": int64(1),
		"cycle": s.cycle, "last_value": s.last, "min_value": int64(1),
		"max_value": s.max, "type_max": s.typeMax, "effective_limit": limit,
		"fraction_used": (s.last - 1) / (limit - 1)}
	if !math.IsNaN(s.ownerMax) {
		r["owner_type_max"] = s.ownerMax
		r["owner_column"] = "public.t.id"
	}
	return r
}

func seqSample(id string, at time.Time, rows ...seqRow) Observation {
	var rs []probes.Row
	for _, r := range rows {
		rs = append(rs, r.row())
	}
	o := obs(id, probes.SequenceRunwayProbe, rs...)
	o.Result.ObservedAt = at
	return o
}

// seqCase: two samples 5 s apart (the subject advancing by step) and the
// runway trend at rate.
func seqCase(s seqRow, step, rate float64) []Observation {
	after := s
	after.last += step
	return []Observation{
		seqSample("E1", t0, s),
		trendsObs("E2", trendRow(probes.RunwaySequence, s.name, 10, after.last,
			limitOf(s), rate, 0.99)),
		seqSample("E3", t0.Add(5*time.Second), after),
	}
}

func limitOf(s seqRow) float64 {
	if !math.IsNaN(s.ownerMax) && s.ownerMax < s.max {
		return s.ownerMax
	}
	return s.max
}

var intSeq = seqRow{name: "public.a_seq", typ: "integer", last: 1.9e9, max: int4Max,
	typeMax: int4Max, ownerMax: int4Max}

func TestSequence_TypeLimit(t *testing.T) {
	d := DiagnoseSequence(seqCase(intSeq, 500, 100), "sequence public.a_seq")
	if d.Family != FamilySequence {
		t.Fatalf("family = %s", d.Family)
	}
	root := requireStatus(t, d, SequenceTypeLimit, StatusRoot)
	if root.Subject != "sequence public.a_seq" || !hasFact(root.Support, "E3", "1900000500") {
		t.Fatalf("type limit root = %+v", root)
	}
	for _, id := range []NodeID{ColumnNarrowerThanSequence, ExplicitMaxvalueLimit} {
		h := requireStatus(t, d, id, StatusRuledOut)
		if !strings.Contains(h.Contradict[0].Text, "binding limit") {
			t.Fatalf("%s contradiction = %+v", id, h.Contradict)
		}
	}
}

func TestSequence_ColumnNarrowerThanSequence(t *testing.T) {
	s := seqRow{name: "public.b_seq", typ: "bigint", last: 2e9, max: int8Max,
		typeMax: int8Max, ownerMax: int4Max}
	d := DiagnoseSequence(seqCase(s, 300, 60), "sequence public.b_seq")
	root := requireStatus(t, d, ColumnNarrowerThanSequence, StatusRoot)
	if !hasFact(root.Support, "E3", "2147483647") {
		t.Fatalf("column root = %+v, want the column's limit", root)
	}
	requireStatus(t, d, SequenceTypeLimit, StatusRuledOut)
}

func TestSequence_ExplicitMaxvalue(t *testing.T) {
	s := seqRow{name: "public.c_seq", typ: "bigint", last: 900000, max: 1e6,
		typeMax: int8Max, ownerMax: int8Max}
	d := DiagnoseSequence(seqCase(s, 10, 2), "sequence public.c_seq")
	requireStatus(t, d, ExplicitMaxvalueLimit, StatusRoot)
}

// Decoy: near its limit but not consuming.
func TestSequence_DormantIsInconclusive(t *testing.T) {
	d := DiagnoseSequence(seqCase(intSeq, 0, 0), "sequence public.a_seq")
	if d.Root != nil {
		t.Fatalf("dormant sequence produced root %+v", d.Root)
	}
	h := requireStatus(t, d, SequenceTypeLimit, StatusRuledOut)
	if !strings.Contains(h.Contradict[0].Text, "did not advance") {
		t.Fatalf("contradiction = %+v", h.Contradict)
	}
}

// Decoy: a cycling sequence wraps at its limit by design.
func TestSequence_CycleIsInconclusive(t *testing.T) {
	s := intSeq
	s.cycle = true
	d := DiagnoseSequence(seqCase(s, 500, 100), "sequence public.a_seq")
	if d.Root != nil {
		t.Fatalf("cycling sequence produced root %+v", d.Root)
	}
	h := requireStatus(t, d, SequenceTypeLimit, StatusRuledOut)
	if !strings.Contains(h.Contradict[0].Text, "CYCLE") {
		t.Fatalf("contradiction = %+v", h.Contradict)
	}
}

// Benign: consuming, but far from the limit at its measured rate.
func TestSequence_FarFromItsLimitIsInconclusive(t *testing.T) {
	s := intSeq
	s.last = 2e7
	d := DiagnoseSequence(seqCase(s, 5, 1), "sequence public.a_seq")
	if d.Root != nil {
		t.Fatalf("far sequence produced root %+v", d.Root)
	}
	requireStatus(t, d, SequenceTypeLimit, StatusAlternative)
}

// A measured rate that reaches the limit within 30 days is near even at
// a small fraction used; exactly half used is near.
func TestSequence_NearBoundaries(t *testing.T) {
	s := intSeq
	s.last = 214748364
	d := DiagnoseSequence(seqCase(s, 5000, 1000), "sequence public.a_seq")
	requireStatus(t, d, SequenceTypeLimit, StatusRoot)
	if !hasObserved(d, "E2", "1932730.28") {
		t.Fatalf("observed = %+v, want the projected seconds", d.Observed)
	}
	half := intSeq
	half.last = (int4Max-1)/2 + 1
	d = DiagnoseSequence(seqCase(half, 1, 1e-6), "sequence public.a_seq")
	requireStatus(t, d, SequenceTypeLimit, StatusRoot)
	half.last -= 2
	d = DiagnoseSequence(seqCase(half, 1, 1e-6), "sequence public.a_seq")
	if d.Root != nil {
		t.Fatalf("just under half used produced root %+v", d.Root)
	}
}

func TestSequence_SubjectNotListed(t *testing.T) {
	d := DiagnoseSequence(seqCase(intSeq, 500, 100), "sequence public.gone_seq")
	if d.Root != nil || !strings.Contains(d.Reason, "public.gone_seq") {
		t.Fatalf("missing subject: root %+v reason %q", d.Root, d.Reason)
	}
}

// Error propagation: an unreadable last value (no privilege) is missing
// evidence, never a sequence at zero.
func TestSequence_UnreadableLastValue(t *testing.T) {
	s := intSeq
	s.last = math.NaN()
	d := DiagnoseSequence(seqCase(s, 0, math.NaN()), "sequence public.a_seq")
	if d.Root != nil || !hasMissing(d, probes.SequenceRunwayProbe, "last_value_unreadable") {
		t.Fatalf("unreadable: root %+v missing %+v", d.Root, d.Missing)
	}
	obs := seqCase(intSeq, 500, 100)
	obs[0] = unavailable("E1", probes.SequenceRunwayProbe, probes.StatusNoPrivilege)
	obs[2] = unavailable("E3", probes.SequenceRunwayProbe, probes.StatusNoPrivilege)
	d = DiagnoseSequence(obs, "sequence public.a_seq")
	if d.Root != nil || !hasMissingStatus(d, probes.SequenceRunwayProbe,
		probes.StatusNoPrivilege) {
		t.Fatalf("denied: root %+v missing %+v", d.Root, d.Missing)
	}
}

// One sample and no trend: consumption is unknown, so nothing is blamed
// and nothing is refuted.
func TestSequence_UnknownConsumption(t *testing.T) {
	d := DiagnoseSequence([]Observation{seqSample("E1", t0, intSeq), trendsObs("E2")},
		"sequence public.a_seq")
	h := requireStatus(t, d, SequenceTypeLimit, StatusAlternative)
	if len(h.Contradict) != 0 || d.Root != nil {
		t.Fatalf("unknown consumption: %+v", h)
	}
}
