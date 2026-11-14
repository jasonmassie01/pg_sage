package autonomy

import (
	"context"
	"testing"
	"time"
)

// vacuum-03 unit tests: the freeze proposal's policy deadline is ahead
// even past the freeze maximum, keeps Kind xid (deadline_overrides keys
// on it) and its evidence names the threat that set it and the server's
// vacuum_failsafe_age. No concurrency test: respond is a pure function of
// the row and the rate sample.

// threatRow is a horizon row at XID age xidAge of a 200 maximum and
// multixact age mxidAge of a 100 maximum.
func threatRow(t *testing.T, xidAge, mxidAge int64) freezeRow {
	t.Helper()
	row, err := scanHorizonRow(adapterRow{values: []any{"public", "events", xidAge,
		int64(200), mxidAge, int64(100), 0.0, int64(1), int64(1), int64(8192),
		(*float64)(nil), false}})
	if err != nil {
		t.Fatalf("scanHorizonRow: %v", err)
	}
	return row
}

func freezeProposalFor(t *testing.T, row freezeRow, rates freezeRateSample) Proposal {
	t.Helper()
	proposal, _, err := NewPostgresFreezeCustodian(nil, "orders", 25).
		respond(context.Background(), row, rates, nil, false)
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	if proposal.Feature != "freeze" || proposal.SQL == "" {
		t.Fatalf("proposal = %#v, want a freeze", proposal)
	}
	return proposal
}

func TestFreezeDeadlinePastMaximumIsAheadAndNamesTheThreat(t *testing.T) {
	rates := freezeRateSample{xid: 1, mxid: 1, known: true, failsafeAge: 1_600_000_000}
	before := time.Now()
	proposal := freezeProposalFor(t, threatRow(t, 201, 1), rates)
	deadline := proposal.Deadline
	if deadline == nil || deadline.Kind != "xid" || deadline.Urgency != "critical" ||
		!deadline.HardAt.After(before) {
		t.Fatalf("policy deadline = %#v, want critical xid ahead of %s", deadline, before)
	}
	if got := proposal.Evidence["deadline_threat"]; got != "xid" {
		t.Fatalf("deadline_threat = %#v, want xid", got)
	}
	if got := proposal.Evidence["vacuum_failsafe_age"]; got != int64(1_600_000_000) {
		t.Fatalf("vacuum_failsafe_age = %#v (%T), want 1600000000", got, got)
	}
}

// Multixacts consumed ten times faster reach the warn limit first and set
// the deadline; the policy kind stays xid and the evidence names
// multixact.
func TestFreezeDeadlineEvidenceNamesTheMultiXactThreat(t *testing.T) {
	proposal := freezeProposalFor(t, threatRow(t, 100, 95),
		freezeRateSample{xid: 1, mxid: 10, known: true})
	if proposal.Deadline == nil || proposal.Deadline.Kind != "xid" {
		t.Fatalf("policy deadline = %#v, want kind xid", proposal.Deadline)
	}
	if got := proposal.Evidence["deadline_threat"]; got != "multixact" {
		t.Fatalf("deadline_threat = %#v, want multixact", got)
	}
	// PG13 and older, or a provider that hides the setting: no failsafe.
	if got, ok := proposal.Evidence["vacuum_failsafe_age"]; ok {
		t.Fatalf("vacuum_failsafe_age = %#v without a measured setting", got)
	}
}

// Without two samples the rate is unknown, so no deadline is claimed and
// no deadline evidence is written.
func TestFreezeDeadlineEvidenceAbsentWithoutAMeasuredRate(t *testing.T) {
	proposal := freezeProposalFor(t, threatRow(t, 201, 1),
		freezeRateSample{xid: 0.001, mxid: 0.001, failsafeAge: 1_600_000_000})
	if proposal.Deadline != nil {
		t.Fatalf("policy deadline = %#v, want none before a rate is measured",
			proposal.Deadline)
	}
	for _, key := range []string{"deadline_threat", "vacuum_failsafe_age"} {
		if got, ok := proposal.Evidence[key]; ok {
			t.Fatalf("%s = %#v without a policy deadline", key, got)
		}
	}
}

// The failsafe setting is read best-effort: missing (PG13, managed
// providers) and unparsable values read as unknown.
func TestParseFailsafeAge(t *testing.T) {
	text := func(s string) *string { return &s }
	tests := []struct {
		name string
		in   *string
		want int64
	}{
		{"PG14 default", text("1600000000"), 1_600_000_000},
		{"missing setting", nil, 0},
		{"empty", text(""), 0},
		{"garbage", text("lots"), 0},
		{"negative", text("-5"), 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseFailsafeAge(test.in); got != test.want {
				t.Fatalf("parseFailsafeAge = %d, want %d", got, test.want)
			}
		})
	}
}
