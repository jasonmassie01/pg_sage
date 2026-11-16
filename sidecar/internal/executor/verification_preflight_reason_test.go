package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// No concurrent access test: joinApprovalReason is pure and shares nothing.

// The preflight's approval reason is added to a reason another guard gave
// (here an operator's rejection), never written over it.
func TestV2_PreflightKeepsAPriorApprovalReason(t *testing.T) {
	const rejected = "an operator rejected this exact change (queue item 7)"
	const weekly = "cannot be verified within 72 h: the targets run 2 times a week"
	detail := map[string]any{analyzer.DetailApprovalRequired: rejected}
	got := joinApprovalReason(detail, weekly)
	if got != rejected+"; "+weekly {
		t.Fatalf("reason %q, want the rejection then the preflight", got)
	}
	if got := joinApprovalReason(map[string]any{analyzer.DetailApprovalRequired: got},
		weekly); got != rejected+"; "+weekly {
		t.Fatalf("a second guard pass repeated the reason: %q", got)
	}
	for _, prior := range []any{nil, true, "  "} {
		d := map[string]any{analyzer.DetailApprovalRequired: prior}
		if got := joinApprovalReason(d, weekly); got != weekly {
			t.Fatalf("prior %#v gave %q, want the preflight reason alone", prior, got)
		}
	}
	if got := joinApprovalReason(nil, weekly); got != weekly {
		t.Fatalf("nil detail gave %q", got)
	}
}
