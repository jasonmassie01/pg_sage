package rca

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// migration-05: a lock_chain finding redirected to an in-flight DDL
// (detail queued_ddl_pid) keeps its blocker identity, which now names the
// DDL so the case playbook offers no signal to the root blocker.
func TestBlockersFromFindings_CarriesQueuedDDL(t *testing.T) {
	findings := []analyzer.Finding{
		{Category: "lock_chain", Detail: map[string]any{"pid": 100,
			"total_blocked": 4, "queued_ddl_pid": 200}},
		{Category: "lock_chain", Detail: map[string]any{"pid": 300,
			"total_blocked": 1}},
		// A wrong-typed marker is ignored, never a panic.
		{Category: "lock_chain", Detail: map[string]any{"pid": 400,
			"total_blocked": 0, "queued_ddl_pid": "200"}},
	}
	bs := blockersFromFindings(findings)
	if len(bs) != 3 {
		t.Fatalf("blockers = %+v, want 3", bs)
	}
	want := map[int]int{100: 200, 300: 0, 400: 0}
	for _, b := range bs {
		if b.QueuedDDLPID != want[b.PID] {
			t.Fatalf("pid %d QueuedDDLPID = %d, want %d", b.PID, b.QueuedDDLPID,
				want[b.PID])
		}
	}
	if d := blockerDescription(bs[0]); !strings.Contains(d, "in-flight DDL pid 200") {
		t.Fatalf("description %q does not name the DDL", d)
	}
	if d := blockerDescription(bs[1]); strings.Contains(d, "DDL") {
		t.Fatalf("description %q names a DDL that is not there", d)
	}
}
