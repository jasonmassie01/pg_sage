package freeze

import (
	"math"
	"strings"
	"testing"
	"time"
)

// vacuum-03: the hard deadline is the server's wraparound warn limit
// (WraparoundWarnAge), not the freeze maximum, and it never overflows;
// urgency stays relative to the freeze maximum. No concurrency test:
// EvaluateHorizon is a pure function of its arguments.

var warnNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

var warnThresholds = Thresholds{RedBufferPct: 25, AmberBufferPct: 50}

func warnSample(edit func(*HorizonSample)) HorizonSample {
	sample := HorizonSample{
		Database: "orders", Schema: "public", Table: "events",
		XIDAge: 100_000_000, XIDMaxAge: 200_000_000, XIDsPerSecond: 100,
		MultiXactAge: 1_000, MultiXactMaxAge: 400_000_000,
		MultiXactsPerSecond: 1,
	}
	edit(&sample)
	return sample
}

func warnDeadline(age int64, rate float64) time.Time {
	seconds := math.Ceil(float64(WraparoundWarnAge-age) / rate)
	return warnNow.Add(time.Duration(seconds) * time.Second)
}

func evaluateWarn(t *testing.T, sample HorizonSample, thresholds Thresholds) Assessment {
	t.Helper()
	assessment, err := EvaluateHorizon(warnNow, sample, thresholds)
	if err != nil {
		t.Fatalf("EvaluateHorizon: %v", err)
	}
	return assessment
}

func TestWraparoundWarnAgeIsTheServerWarnLimit(t *testing.T) {
	if WraparoundWarnAge != 2_107_483_647 {
		t.Fatalf("WraparoundWarnAge = %d, want 2^31-1-40M", WraparoundWarnAge)
	}
}

// Past autovacuum_freeze_max_age, the most urgent case, the deadline was
// clamped to now, so the documented override and budget bypass (which
// need a deadline still ahead) never fired.
func TestHorizonPastFreezeMaximumKeepsAFutureHardDeadline(t *testing.T) {
	age := int64(200_000_001)
	assessment := evaluateWarn(t, warnSample(func(s *HorizonSample) {
		s.XIDAge = age
	}), warnThresholds)
	if assessment.XID.Distance != -1 || assessment.Urgency != UrgencyRed {
		t.Fatalf("XID distance=%d urgency=%q, want -1 and red (urgency is still "+
			"relative to the freeze maximum)", assessment.XID.Distance, assessment.Urgency)
	}
	want := warnDeadline(age, 100)
	got := assessment.Deadline
	if !got.HardAt.After(warnNow) || !got.HardAt.Equal(want) ||
		got.Threat != ThreatXID || got.Kind != DeadlineXID || got.Urgency != UrgencyRed {
		t.Fatalf("Deadline = %#v, want XID red at %s", got, want)
	}
	if !assessment.Proposal.Deadline.HardAt.Equal(want) ||
		assessment.Proposal.Threat != ThreatXID {
		t.Fatalf("Proposal deadline = %#v, want %s", assessment.Proposal.Deadline, want)
	}
}

// The custodian's 0.001/s rate floor over a 4e8 distance is 4e11 s;
// seconds*1e9 overflowed time.Duration (amd64: MinInt64, a deadline about
// 292 years ago that then won the earlier-deadline race for every red XID
// table).
func TestHorizonFloorRateDoesNotOverflowTheDeadline(t *testing.T) {
	ceiling := warnNow.AddDate(101, 0, 0)
	for _, mxidAge := range []int64{0, WraparoundWarnAge - 400_000_000} {
		assessment := evaluateWarn(t, warnSample(func(s *HorizonSample) {
			s.XIDAge = 160_000_000
			s.MultiXactAge = mxidAge
			s.MultiXactMaxAge = 2_000_000_000
			s.MultiXactsPerSecond = 0.001
		}), warnThresholds)
		multi := assessment.MultiXact.HardAt
		if !multi.After(warnNow) || !multi.Before(ceiling) {
			t.Fatalf("mxid age %d: multixact HardAt = %s, want in (now, now+101y)",
				mxidAge, multi)
		}
		want := warnDeadline(160_000_000, 100)
		if assessment.Deadline.Threat != ThreatXID || !assessment.Deadline.HardAt.Equal(want) {
			t.Fatalf("mxid age %d: Deadline = %#v, want the XID deadline %s",
				mxidAge, assessment.Deadline, want)
		}
	}
}

// Both counters at the rate floor clamp to the same 100-year ceiling; the
// XID threat is kept on a tie.
func TestHorizonClampsBothDeadlinesAndKeepsXIDOnATie(t *testing.T) {
	assessment := evaluateWarn(t, warnSample(func(s *HorizonSample) {
		s.XIDAge, s.XIDsPerSecond = 160_000_000, 0.001
		s.MultiXactAge, s.MultiXactsPerSecond = 0, 0.001
	}), warnThresholds)
	xid, multi := assessment.XID.HardAt, assessment.MultiXact.HardAt
	if !xid.Equal(multi) || !xid.After(warnNow.AddDate(99, 0, 0)) ||
		!xid.Before(warnNow.AddDate(101, 0, 0)) {
		t.Fatalf("clamped deadlines xid=%s multixact=%s, want both at about 100 years",
			xid, multi)
	}
	if assessment.Deadline.Threat != ThreatXID {
		t.Fatalf("tie threat = %q, want xid", assessment.Deadline.Threat)
	}
}

// At and past WraparoundWarnAge the server is already warning: the
// urgency is red whatever the freeze maximum says, and the deadline is
// the runway to WraparoundStopAge (where the server stops assigning
// XIDs) at the measured rate. A one-second deadline would lapse while the
// scan's earlier proposals run, refusing the override for every later
// table. At or past the stop limit the deadline is one second ahead. One
// XID short of the warn limit, the deadline is the warn-limit runway.
func TestHorizonAtOrPastWraparoundWarnAgeIsRedAndImminent(t *testing.T) {
	// A maximum above the warn limit and 1%/2% buffers make the urgency
	// from the freeze maximum alone amber at the warn limit.
	thresholds := Thresholds{RedBufferPct: 1, AmberBufferPct: 2}
	stopRunway := func(age int64) time.Time {
		return warnNow.Add(time.Duration(math.Ceil(float64(WraparoundStopAge-age)/0.5)) *
			time.Second)
	}
	tests := []struct {
		name    string
		age     int64
		urgency Urgency
		hardAt  time.Time
	}{
		{"one short of the warn limit", WraparoundWarnAge - 1, UrgencyAmber,
			warnNow.Add(2 * time.Second)},
		{"at the warn limit", WraparoundWarnAge, UrgencyRed, stopRunway(WraparoundWarnAge)},
		{"past the warn limit", WraparoundWarnAge + 10_000_000, UrgencyRed,
			warnNow.Add(54_000_000 * time.Second)},
		{"one short of the stop limit", WraparoundStopAge - 1, UrgencyRed,
			warnNow.Add(2 * time.Second)},
		{"at the stop limit", WraparoundStopAge, UrgencyRed, warnNow.Add(time.Second)},
		{"past the stop limit", WraparoundStopAge + 1_000_000, UrgencyRed,
			warnNow.Add(time.Second)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assessment := evaluateWarn(t, warnSample(func(s *HorizonSample) {
				s.XIDAge, s.XIDMaxAge, s.XIDsPerSecond = test.age, math.MaxInt32, 0.5
			}), thresholds)
			if assessment.XID.Urgency != test.urgency || assessment.Urgency != test.urgency {
				t.Fatalf("urgency xid=%q overall=%q, want %q",
					assessment.XID.Urgency, assessment.Urgency, test.urgency)
			}
			if !assessment.XID.HardAt.Equal(test.hardAt) ||
				!assessment.Deadline.HardAt.Equal(test.hardAt) {
				t.Fatalf("HardAt xid=%s deadline=%s, want %s", assessment.XID.HardAt,
					assessment.Deadline.HardAt, test.hardAt)
			}
		})
	}
}

func TestWraparoundStopAgeIsTheServerStopLimit(t *testing.T) {
	if WraparoundStopAge != 2_144_483_647 || WraparoundStopAge <= WraparoundWarnAge {
		t.Fatalf("WraparoundStopAge = %d, want 2^31-1-3M, above the warn limit",
			WraparoundStopAge)
	}
}

// The multixact threat is the deadline only when its warn-limit deadline
// is earlier; a red multixact horizon with a later deadline leaves the
// XID threat in charge.
func TestHorizonPicksMultiXactOnlyWhenItsDeadlineIsEarlier(t *testing.T) {
	tests := []struct {
		name   string
		rate   float64
		threat ThreatKind
		hardAt time.Time
	}{
		{"fast multixact consumption", 1000, ThreatMultiXact,
			warnDeadline(300_000_000, 1000)},
		{"slow multixact consumption", 10, ThreatXID, warnDeadline(160_000_000, 100)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assessment := evaluateWarn(t, warnSample(func(s *HorizonSample) {
				s.XIDAge = 160_000_000
				s.MultiXactAge, s.MultiXactsPerSecond = 300_000_000, test.rate
			}), warnThresholds)
			if assessment.MultiXact.Urgency != UrgencyRed {
				t.Fatalf("multixact urgency = %q, want red", assessment.MultiXact.Urgency)
			}
			got := assessment.Deadline
			if got.Threat != test.threat || !got.HardAt.Equal(test.hardAt) ||
				got.Kind != DeadlineXID || assessment.Proposal.Threat != test.threat {
				t.Fatalf("Deadline = %#v proposal threat %q, want %q at %s",
					got, assessment.Proposal.Threat, test.threat, test.hardAt)
			}
		})
	}
}

func TestHorizonRejectsUnusableRates(t *testing.T) {
	tests := []struct {
		name string
		edit func(*HorizonSample)
		want string
	}{
		{"negative xid rate", func(s *HorizonSample) { s.XIDsPerSecond = -1 }, "xid rate"},
		{"NaN xid rate", func(s *HorizonSample) { s.XIDsPerSecond = math.NaN() }, "xid rate"},
		{"infinite xid rate", func(s *HorizonSample) { s.XIDsPerSecond = math.Inf(1) },
			"xid rate"},
		{"zero multixact rate", func(s *HorizonSample) { s.MultiXactsPerSecond = 0 },
			"multixact rate"},
		{"NaN multixact rate", func(s *HorizonSample) {
			s.MultiXactsPerSecond = math.NaN()
		}, "multixact rate"},
		{"infinite multixact rate", func(s *HorizonSample) {
			s.MultiXactsPerSecond = math.Inf(1)
		}, "multixact rate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assessment, err := EvaluateHorizon(warnNow, warnSample(test.edit), warnThresholds)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if !assessment.Deadline.HardAt.IsZero() || assessment.Proposal.Intent != "" {
				t.Fatalf("unusable rate produced an assessment %#v", assessment)
			}
		})
	}
}
