package autonomy

import (
	"context"
	"strings"
	"testing"
)

// vacuum-07: the custodian's autovacuum tuner is gone, so freeze amber
// proposes nothing, cycle after cycle (before, every scan re-proposed
// scale factor 0.02, fired the schema guard and was charged to the
// performance budget). No concurrency test: one custodian per database
// scans in a serial loop.

func amberRow(t *testing.T, table string, dead float64) freezeRow {
	t.Helper()
	row, err := scanHorizonRow(adapterRow{values: []any{
		"public", table, int64(60), int64(100), int64(1), int64(100), dead,
	}})
	if err != nil {
		t.Fatalf("scanHorizonRow: %v", err)
	}
	return row
}

func TestFreezeAmberTableGetsNoCustodianProposalAcrossCycles(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25)
	rows := []freezeRow{amberRow(t, "orders", 0.3), amberRow(t, "events", 1)}
	alters := 0
	for cycle := 1; cycle <= 2; cycle++ {
		proposals, err := custodian.plan(context.Background(), rows, knownRates, nil, false)
		if err != nil {
			t.Fatalf("cycle %d: plan: %v", cycle, err)
		}
		for _, proposal := range proposals {
			if strings.Contains(strings.ToUpper(proposal.SQL), "ALTER TABLE") {
				alters++
			}
		}
		if len(proposals) != 0 {
			t.Fatalf("cycle %d: proposals = %#v, want none for amber tables", cycle, proposals)
		}
	}
	if alters != 0 {
		t.Fatalf("custodian proposed %d ALTER TABLE actions over two cycles, want 0", alters)
	}
}

// An amber row still runs through the urgency check: the same table at
// red is frozen, so the empty amber answer is not a broken scan.
func TestFreezeAmberToRedStillFreezes(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25)
	red, err := scanHorizonRow(adapterRow{values: []any{
		"public", "orders", int64(90), int64(100), int64(1), int64(100), 0.3,
	}})
	if err != nil {
		t.Fatalf("scanHorizonRow: %v", err)
	}
	for _, step := range []struct {
		row  freezeRow
		want int
	}{{amberRow(t, "orders", 0.3), 0}, {red, 1}} {
		proposals, err := custodian.plan(context.Background(),
			[]freezeRow{step.row}, knownRates, nil, false)
		if err != nil || len(proposals) != step.want {
			t.Fatalf("plan = %#v, %v; want %d proposals", proposals, err, step.want)
		}
		if step.want == 1 && (proposals[0].Feature != "freeze" ||
			proposals[0].SQL != `VACUUM (FREEZE) "public"."orders"`) {
			t.Fatalf("red proposal = %#v, want a freeze", proposals[0])
		}
	}
}

// A zero-value response kind maps to the freeze feature with no SQL; the
// tuning feature name is never produced.
func TestFreezeResponseProposalNeverNamesAutovacuumTuning(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25)
	for _, dead := range []float64{0, 0.2, 0.99} {
		proposal, _, err := custodian.respond(context.Background(),
			amberRow(t, "orders", dead), knownRates, nil, false)
		if err != nil {
			t.Fatalf("dead %v: respond: %v", dead, err)
		}
		if proposal.Feature == "autovacuum_tuning" || proposal.SQL != "" {
			t.Fatalf("dead %v: proposal = %#v", dead, proposal)
		}
	}
}
