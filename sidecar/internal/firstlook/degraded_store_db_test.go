package firstlook

import (
	"strings"
	"testing"
)

// TestStoredReportKeepsDegradedSequenceGrant runs the first look as a role
// that cannot read sequences and reads the report back from the store: the
// degraded check, its GRANT and the truncation flag must survive the round
// trip, since the dashboard and the onboarding API read only the store.
func TestStoredReportKeepsDegradedSequenceGrant(t *testing.T) {
	admin, ctx := livePool(t)
	s := seedProblems(t, ctx, admin)
	mon := monitorRole(t, ctx, admin)
	opts := testOptions("fl_degraded_" + s)
	opts.Thresholds.MaxItemsPerRule = 1
	r, err := Run(ctx, mon, opts)
	if err != nil {
		t.Fatalf("run as monitor: %v", err)
	}
	store := NewStore(admin)
	if _, err := store.Save(ctx, r); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := store.Latest(ctx, opts.Database)
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	c := checkStatus(got, RuleSequenceRunway)
	if c.Status != CheckDegraded || !strings.Contains(c.Note,
		"GRANT SELECT ON ALL SEQUENCES IN SCHEMA") {
		t.Fatalf("stored sequence check = %+v, want degraded with the GRANT", c)
	}
	counts := CountChecks(got.Checks, "")
	if counts.Degraded < 1 || counts.Total < 9 {
		t.Fatalf("stored catalog counts = %+v, want >= 1 degraded of >= 9", counts)
	}
	if counts != CountChecks(r.Checks, "") {
		t.Fatalf("stored counts %+v differ from the run's %+v", counts,
			CountChecks(r.Checks, ""))
	}
	// The seed has two duplicate indexes on child.a and a cap of 1 per rule:
	// some catalog rule found more than it shows, and the flag is stored.
	if counts.Truncated < 1 {
		t.Fatalf("stored checks %+v: no truncated check under a cap of 1", got.Checks)
	}
}
