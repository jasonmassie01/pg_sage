package tuning

import (
	"strings"
	"testing"
)

// market-15b: on PostgreSQL 18 the agent's table context says that skip
// scan can serve a non-leading key of a low-cardinality composite; before
// 18 the packet is unchanged.

// No concurrency or state-transition tests: the note is a pure function
// of the table context, written once per packet.

func TestPacket_SkipScanNoteOnPG18Only(t *testing.T) {
	h := newHarness(t)
	tc := ordersContext()
	tc.PGMajor = 18
	h.indexes.contexts["public.orders"] = tc
	pk := ordersPacket(t, h, nil)
	if strings.Count(pk.Text, "skip scan") != 1 || !strings.Contains(pk.Text, "PostgreSQL 18") {
		t.Fatalf("PG 18 packet lacks exactly one skip-scan note:\n%s", pk.Text)
	}

	h17 := newHarness(t)
	before := ordersPacket(t, h17, nil).Text
	tc.PGMajor = 17
	h17.indexes.contexts["public.orders"] = tc
	if got := ordersPacket(t, h17, nil).Text; got != before ||
		strings.Contains(got, "skip scan") {
		t.Fatalf("PG 17 packet changed or has the note:\n%s", got)
	}
}
