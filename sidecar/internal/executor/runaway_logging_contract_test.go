package executor

import (
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access: each case owns its tracker; concurrent Evaluate has existing coverage.
// No SQL integration is needed: state transitions emit diagnostics and do not execute queries.
func TestAuditRunawayLogsPreservePIDAndPolicy(t *testing.T) {
	cases := []struct{ before, after, level, event string }{
		{"", "warned", "WARN", "warned"},
		{"warned", "cancelled", "WARN", "escalated to cancel"},
		{"cancelled", "terminated", "ERROR", "escalated to terminate"},
	}
	for _, tc := range cases {
		t.Run(tc.after, func(t *testing.T) {
			var message string
			tracker := NewRunawayTracker(defaultCfg(), 1,
				func(level, format string, args ...any) {
					message = level + ":" + fmt.Sprintf(format, args...)
				})
			query := &TrackedQuery{PID: 42, MatchedPolicy: "audit-policy", State: tc.before}
			tracker.cycle = 3
			tracker.advanceState(query, &config.RunawayPolicy{WarnCycles: 1, CancelCycles: 1})
			want := tc.level + ":runaway query " + tc.event + " pid=42 policy=audit-policy"
			if query.State != tc.after || message != want {
				t.Fatalf("state=%q message=%q; want state=%q message=%q",
					query.State, message, tc.after, want)
			}
		})
	}
}
