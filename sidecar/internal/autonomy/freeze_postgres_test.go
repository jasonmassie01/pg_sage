package autonomy

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
)

func TestFreezePolicyDeadlineOnlyAllowsRedOverride(t *testing.T) {
	hardAt := time.Now().Add(time.Hour)
	red := freezePolicyDeadline(freeze.Proposal{
		Urgency:  freeze.UrgencyRed,
		Deadline: freeze.Deadline{HardAt: hardAt},
	})
	if red == nil || red.Kind != policy.DeadlineXID ||
		red.Urgency != policy.UrgencyCritical || !red.HardAt.Equal(hardAt) {
		t.Fatalf("red deadline = %#v", red)
	}
	if amber := freezePolicyDeadline(freeze.Proposal{
		Urgency: freeze.UrgencyAmber,
	}); amber != nil {
		t.Fatalf("amber deadline = %#v, want no override", amber)
	}
}

// vacuum-09 boundaries at production magnitudes: the earlier unit rows
// used ages of at most 100 and rates of 1/s, so overflow, past-limit and
// multixact selection were never exercised. No concurrency test: respond
// is a pure function of one row and one rate sample.

// realRow is a horizon row with real maxima (2e9 for both counters).
func realRow(t *testing.T, xidAge, mxidAge int64) freezeRow {
	t.Helper()
	row, err := scanHorizonRow(adapterRow{values: []any{"public", "ledger", xidAge,
		int64(2_000_000_000), mxidAge, int64(2_000_000_000), 0.0, int64(9),
		int64(1), int64(8192), (*float64)(nil), false}})
	if err != nil {
		t.Fatalf("scanHorizonRow: %v", err)
	}
	return row
}

// realRates is a busy primary: a thousand XIDs and multixacts a second.
var realRates = freezeRateSample{xid: 1000, mxid: 1000, known: true}

// An age of 2^31-1 is past the wraparound stop limit: the deadline is one
// second ahead (the server is about to refuse writes), never a time that
// wrapped into the past or the far future.
func TestFreezeDeadlineAtMaximumInt32AgeIsNowNotWrapped(t *testing.T) {
	before := time.Now()
	proposal := freezeProposalFor(t, realRow(t, 2147483647, 1), realRates)
	after := time.Now()
	d := proposal.Deadline
	if d == nil || d.Urgency != policy.UrgencyCritical {
		t.Fatalf("deadline = %#v, want a critical override", d)
	}
	if d.HardAt.Before(before) || d.HardAt.After(after.Add(2*time.Second)) {
		t.Fatalf("HardAt = %s, want within 2s after %s (now, not wrapped)", d.HardAt, before)
	}
	if proposal.SQL != `VACUUM (FREEZE) "public"."ledger"` {
		t.Fatalf("SQL = %q, want the table's freeze", proposal.SQL)
	}
}

// Ahead of the warn limit the deadline is the remaining distance at the
// measured rate: (2^31-1-40M-1.9e9)/1000 s, exact to the second, with no
// int64 or Duration overflow at these magnitudes.
func TestFreezeDeadlineRealMagnitudeIsTheRunwayAtTheRate(t *testing.T) {
	const age = 1_900_000_000
	want := time.Duration((freeze.WraparoundWarnAge-age)/1000) * time.Second
	before := time.Now()
	proposal := freezeProposalFor(t, realRow(t, age, 1), realRates)
	after := time.Now()
	d := proposal.Deadline
	if d == nil {
		t.Fatal("no policy deadline for a red table with a known rate")
	}
	if d.HardAt.Before(before.Add(want)) || d.HardAt.After(after.Add(want+time.Second)) {
		t.Fatalf("HardAt = %s, want now + %s", d.HardAt, want)
	}
	if got := proposal.Evidence["deadline_threat"]; got != "xid" {
		t.Fatalf("deadline_threat = %#v, want xid", got)
	}
}

// An age past the freeze maximum (2e9 of a 2e9 maximum, below the warn
// limit) is red with the deadline still ahead.
func TestFreezeDeadlinePastTheMaximumStaysAhead(t *testing.T) {
	before := time.Now()
	proposal := freezeProposalFor(t, realRow(t, 2_050_000_000, 1), realRates)
	d := proposal.Deadline
	if d == nil || !d.HardAt.After(before) ||
		d.HardAt.After(before.Add(100*24*time.Hour)) {
		t.Fatalf("deadline = %#v, want ahead of %s and within days", d, before)
	}
}

// Multixact is the threat when its deadline is earlier, at real ages.
func TestFreezeDeadlineSelectsMultiXactAtRealMagnitudes(t *testing.T) {
	const mxidAge = 1_950_000_000
	proposal := freezeProposalFor(t, realRow(t, 100_000_000, mxidAge), realRates)
	if got := proposal.Evidence["deadline_threat"]; got != "multixact" {
		t.Fatalf("deadline_threat = %#v, want multixact", got)
	}
	want := time.Duration((freeze.WraparoundWarnAge-mxidAge)/1000) * time.Second
	if d := proposal.Deadline; d == nil || d.HardAt.After(time.Now().Add(want+time.Second)) {
		t.Fatalf("deadline = %#v, want the multixact runway %s", d, want)
	}
}

// The smallest measurable rate with the largest distance is capped at
// 100 years rather than overflowing time.Duration into the past.
func TestFreezeDeadlineSlowRateIsCappedNotOverflowed(t *testing.T) {
	before := time.Now()
	slow := freezeRateSample{xid: 1e-9, mxid: 1e-9, known: true}
	proposal := freezeProposalFor(t, realRow(t, 1_600_000_000, 1), slow)
	d := proposal.Deadline
	if d == nil || !d.HardAt.After(before.Add(99*365*24*time.Hour)) ||
		d.HardAt.After(before.Add(101*365*24*time.Hour)) {
		t.Fatalf("deadline = %#v, want capped at 100 years", d)
	}
}

// A green table at real magnitudes yields nothing to route.
func TestFreezeGreenRealMagnitudeProposesNothing(t *testing.T) {
	proposal, _, err := NewPostgresFreezeCustodian(nil, "orders", 25).
		respond(context.Background(), realRow(t, 1_000_000, 1_000_000), realRates, nil, false)
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	if proposal.SQL != "" || proposal.Deadline != nil || proposal.ResponseKind != "" {
		t.Fatalf("green proposal = %#v, want none", proposal)
	}
}
