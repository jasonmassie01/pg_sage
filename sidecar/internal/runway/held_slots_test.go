package runway

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-02 review: while the replication slots are unreadable, an open
// slot finding whose series has no sample in the lookback is not a
// cleared runway (the slot may still exist and retain WAL), so it is held
// too. A measured slot series still resolves as before.
//
// No concurrency test: Holds is a pure function of its input.

func TestHolds_UnreadableSlotsHoldOpenSlotFindingsWithoutSamples(t *testing.T) {
	opts := testOptions()
	measuredSlot := trend(probes.RunwayWALSlot, "steady", 12, time.Hour, 1e6, 10<<30, 0, 1)
	short := trend(probes.RunwayWALSlot, "young", 2, time.Minute, 5e9, 10<<30, 1, 1)
	in := EvalInput{Trends: []probes.RunwayTrend{measuredSlot, short}, TrendsOK: true,
		SlotsOK: false, OpenSlots: []string{"cdc", "steady", "young"}}
	h := Holds(in, opts)
	want := map[string]string{"slot:cdc": HoldNoSamples, "slot:young": HoldUnmeasured}
	if len(h[CategoryWAL]) != len(want)+1 { // + disk: no row in a complete read
		t.Fatalf("WAL holds %v, want %v plus disk", h[CategoryWAL], want)
	}
	for ident, reason := range want {
		if got := heldReason(h, CategoryWAL, ident); got != reason {
			t.Errorf("%s held %q, want %q", ident, got, reason)
		}
	}
	if got, ok := h[CategoryWAL]["slot:steady"]; ok {
		t.Errorf("a measured slot series was held (%q); it must be free to resolve", got)
	}
}

// Boundary: a readable reading ignores OpenSlots (a slot that is gone
// resolves), and a truncated read that may have cut wal_slot guesses no
// "no_samples" hold (WAL is not evaluated then anyway).
func TestHolds_OpenSlotsOnlyWhileUnreadableAndComplete(t *testing.T) {
	opts := testOptions()
	in := EvalInput{TrendsOK: true, SlotsOK: true, Slots: nil, OpenSlots: []string{"cdc"}}
	if got, ok := Holds(in, opts)[CategoryWAL]["slot:cdc"]; ok {
		t.Errorf("readable slots without cdc held it (%q); a dropped slot resolves", got)
	}
	in = EvalInput{TrendsOK: true, SlotsOK: false, OpenSlots: []string{"cdc"},
		TrendsTruncated: true, TrendsLastKind: probes.RunwaySequence,
		Trends: []probes.RunwayTrend{trend(probes.RunwaySequence, "public.s", 12,
			time.Hour, 1, 2147483647, 0, 1)}}
	if got, ok := Holds(in, opts)[CategoryWAL]["slot:cdc"]; ok {
		t.Errorf("a read cut before wal_slot guessed slot:cdc held (%q)", got)
	}
	if _, evaluated := Evaluate(in, opts); hasCategory(evaluated, CategoryWAL) {
		t.Errorf("a read cut before wal_slot evaluated WAL (%v)", evaluated)
	}
}

// Nil/empty: unreadable slots with no open slot finding and no slot rows
// hold no slot.
func TestHolds_UnreadableSlotsWithNothingOpenHoldNoSlot(t *testing.T) {
	h := Holds(EvalInput{TrendsOK: true, SlotsOK: false}, testOptions())
	for ident := range h[CategoryWAL] {
		if ident != "disk" {
			t.Errorf("nothing open, yet held %s (%q)", ident, h[CategoryWAL][ident])
		}
	}
}
