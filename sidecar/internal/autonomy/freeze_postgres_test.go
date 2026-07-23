package autonomy

import (
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
