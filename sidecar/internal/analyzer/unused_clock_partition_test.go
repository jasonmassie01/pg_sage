package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-03 x rules-04: the clock's warmup hold keeps an open zero-scan
// finding open until the rule emits it again. The rule never emits an
// index attached to a partitioned index, so a hold on one would keep its
// rejected DROP INDEX CONCURRENTLY open until the sidecar restarts.

// No concurrent access tests: see unused_clock_test.go.

// attachedClockSnap is clockSnap with the index attached to the
// partitioned index parent.
func attachedClockSnap(oid uint32, parent uint32) *collector.Snapshot {
	snap := clockSnap(oid, 0)
	snap.Indexes[0].ParentIndexOID = parent
	return snap
}

// Upgrade: an open finding on an attached child (raised before rules-03)
// is not held; it resolves on the first cycle.
func TestUnusedClock_AttachedChildFindingNotHeld(t *testing.T) {
	h := newClockHarness(&fakeClockDB{open: []string{clockIdent}})
	got, res := h.cycle(attachedClockSnap(100, 50))
	if len(got) != 0 {
		t.Fatalf("findings = %v, want none for an attached child", identsOf(got))
	}
	if !res["unused_index"] || h.kept()[clockIdent] {
		t.Fatalf("resolvable=%v kept=%v, want the child's finding to resolve",
			res["unused_index"], h.kept())
	}
	if len(h.a.unusedClock.held) != 0 {
		t.Fatalf("held = %v, want no hold on an attached child", h.a.unusedClock.held)
	}
}

// State transition: held standalone -> attached. Attaching the index to a
// partitioned index ends its hold, so the finding resolves next cycle.
func TestUnusedClock_AttachEndsHold(t *testing.T) {
	h := newClockHarness(&fakeClockDB{open: []string{clockIdent}})
	h.cycle(clockSnap(100, 0))
	if !h.kept()[clockIdent] {
		t.Fatalf("standalone finding not held: kept=%v", h.kept())
	}
	h.now = clockNow.Add(time.Hour)
	got, res := h.cycle(attachedClockSnap(100, 50))
	if len(got) != 0 || !res["unused_index"] || h.kept()[clockIdent] {
		t.Fatalf("after attach: findings=%v resolvable=%v kept=%v, want resolved",
			identsOf(got), res["unused_index"], h.kept())
	}
	if len(h.a.unusedClock.held) != 0 {
		t.Fatalf("hold survived the attach: %v", h.a.unusedClock.held)
	}
}
