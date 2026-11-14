package freeze

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestEvaluateHorizonComputesXIDDistanceAndGraduatedUrgency(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		age      int64
		wantDist int64
		want     Urgency
	}{
		{"green above amber buffer", 80, 120, UrgencyGreen},
		{"amber at half remaining", 100, 100, UrgencyAmber},
		{"red at twenty five percent remaining", 150, 50, UrgencyRed},
		{"overdue remains red", 220, -20, UrgencyRed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assessment, err := EvaluateHorizon(now, HorizonSample{
				Database: "orders", Schema: "public", Table: "events",
				XIDAge: test.age, XIDMaxAge: 200, XIDsPerSecond: 10,
				MultiXactAge: 20, MultiXactMaxAge: 400,
				MultiXactsPerSecond: 1,
			}, Thresholds{RedBufferPct: 25, AmberBufferPct: 50})
			if err != nil {
				t.Fatalf("EvaluateHorizon: %v", err)
			}
			if assessment.XID.Distance != test.wantDist {
				t.Fatalf("XID distance = %d, want %d",
					assessment.XID.Distance, test.wantDist)
			}
			if assessment.Urgency != test.want {
				t.Fatalf("Urgency = %q, want %q", assessment.Urgency, test.want)
			}
		})
	}
}

func TestEvaluateHorizonBuildsDeterministicDeadlineFromConsumptionRate(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	assessment, err := EvaluateHorizon(now, HorizonSample{
		Database: "orders", Schema: "public", Table: "events",
		XIDAge: 150, XIDMaxAge: 200, XIDsPerSecond: 10,
		MultiXactAge: 10, MultiXactMaxAge: 1000,
		MultiXactsPerSecond: 1,
	}, Thresholds{RedBufferPct: 25, AmberBufferPct: 50})
	if err != nil {
		t.Fatalf("EvaluateHorizon: %v", err)
	}
	// The deadline is the runway to the wraparound warn limit at the rate
	// (vacuum-03), not to the freeze maximum: ceil((W-150)/10) seconds.
	wantHardAt := now.Add(time.Duration(math.Ceil(float64(WraparoundWarnAge-150)/10)) *
		time.Second)
	if assessment.Deadline.Kind != DeadlineXID ||
		assessment.Deadline.Threat != ThreatXID ||
		assessment.Deadline.Urgency != UrgencyRed ||
		!assessment.Deadline.HardAt.Equal(wantHardAt) {
		t.Fatalf("Deadline = %#v, want XID red at %s",
			assessment.Deadline, wantHardAt)
	}
	if assessment.Deadline.Remaining != 50 {
		t.Fatalf("deadline remaining = %d, want 50", assessment.Deadline.Remaining)
	}
}

func TestEvaluateHorizonUsesEarlierMultiXactDeadline(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	assessment, err := EvaluateHorizon(now, HorizonSample{
		Database: "orders", Schema: "public", Table: "shared_rows",
		XIDAge: 100, XIDMaxAge: 1000, XIDsPerSecond: 10,
		MultiXactAge: 180, MultiXactMaxAge: 200,
		MultiXactsPerSecond: 10,
	}, Thresholds{RedBufferPct: 25, AmberBufferPct: 50})
	if err != nil {
		t.Fatalf("EvaluateHorizon: %v", err)
	}
	if assessment.MultiXact.Distance != 20 || assessment.Urgency != UrgencyRed {
		t.Fatalf("MultiXact assessment = %#v", assessment)
	}
	if assessment.Deadline.Kind != DeadlineXID ||
		assessment.Deadline.Threat != ThreatMultiXact ||
		!assessment.Deadline.HardAt.Equal(now.Add(time.Duration(math.Ceil(
			float64(WraparoundWarnAge-180)/10))*time.Second)) {
		t.Fatalf("MultiXact deadline = %#v", assessment.Deadline)
	}
	if assessment.Proposal.Intent != IntentVacuumFreeze ||
		assessment.Proposal.Threat != ThreatMultiXact ||
		assessment.Proposal.Deadline != assessment.Deadline {
		t.Fatalf("Proposal = %#v", assessment.Proposal)
	}
}

func TestEvaluateHorizonFailsClosedForInvalidOrUnmeasurableInputs(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range invalidHorizonCases() {
		t.Run(test.name, func(t *testing.T) {
			assessment, err := EvaluateHorizon(now, test.sample,
				Thresholds{RedBufferPct: 25, AmberBufferPct: 50})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if assessment.Proposal.Intent != "" || !assessment.Deadline.HardAt.IsZero() {
				t.Fatalf("invalid input produced actionable assessment %#v", assessment)
			}
		})
	}
}

type invalidHorizonCase struct {
	name   string
	sample HorizonSample
	want   string
}

func invalidHorizonCases() []invalidHorizonCase {
	return []invalidHorizonCase{
		{
			name: "missing identity",
			sample: HorizonSample{
				XIDAge: 10, XIDMaxAge: 200, XIDsPerSecond: 1,
				MultiXactAge: 10, MultiXactMaxAge: 200,
				MultiXactsPerSecond: 1,
			},
			want: "identity",
		},
		{
			name: "nonpositive XID maximum",
			sample: validHorizonSample(func(sample *HorizonSample) {
				sample.XIDMaxAge = 0
			}),
			want: "xid maximum",
		},
		{
			name: "missing XID rate",
			sample: validHorizonSample(func(sample *HorizonSample) {
				sample.XIDsPerSecond = 0
			}),
			want: "xid rate",
		},
		{
			name: "missing multixact rate",
			sample: validHorizonSample(func(sample *HorizonSample) {
				sample.MultiXactsPerSecond = 0
			}),
			want: "multixact rate",
		},
	}
}

func validHorizonSample(edit func(*HorizonSample)) HorizonSample {
	sample := HorizonSample{
		Database: "orders", Schema: "public", Table: "events",
		XIDAge: 150, XIDMaxAge: 200, XIDsPerSecond: 10,
		MultiXactAge: 150, MultiXactMaxAge: 200,
		MultiXactsPerSecond: 10,
	}
	edit(&sample)
	return sample
}

// vacuum-11b: the fallback agrees with config validation, which accepts
// only 0 < red < 100. Values inside the open interval, up to its edges,
// are used as given; the amber rule min(2 x red, 100) is unchanged.
func TestBufferThresholdsUsesOpenIntervalUpToItsEdges(t *testing.T) {
	tests := []struct{ red, wantRed, wantAmber float64 }{
		{0.0001, 0.0001, 0.0002},
		{0.5, 0.5, 1},
		{49.9, 49.9, 99.8},
		{99.999, 99.999, 100},
	}
	for _, tt := range tests {
		got := BufferThresholds(tt.red)
		if got.RedBufferPct != tt.wantRed || got.AmberBufferPct != tt.wantAmber {
			t.Fatalf("BufferThresholds(%v) = %+v, want %v/%v",
				tt.red, got, tt.wantRed, tt.wantAmber)
		}
	}
}

// NaN compares false with every bound, so a range check written as
// "redPct <= 0 || redPct >= 100" let it through as NaN thresholds, under
// which no table is ever red or amber. The defence falls back instead.
func TestBufferThresholdsFallsBackOnNaNAndInfinity(t *testing.T) {
	for _, red := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		got := BufferThresholds(red)
		if got.RedBufferPct != 25 || got.AmberBufferPct != 50 {
			t.Fatalf("BufferThresholds(%v) = %+v, want the 25/50 fallback", red, got)
		}
		const maximum = int64(200_000_000)
		if u := TableUrgency(maximum, maximum, 0, maximum, got); u != UrgencyRed {
			t.Fatalf("exhausted runway under BufferThresholds(%v) = %q, want red", red, u)
		}
	}
}
