package earned

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/ha"
)

// The HA adapter probes the role on every read, so a downgrade sees the
// role at authorization time, not a cached one.
func TestHAMonitorSourceProbesTheLiveRole(t *testing.T) {
	pool := testPool(t)
	src := NewHAMonitorSource(ha.New(pool, func(string, string, ...any) {}))
	st, err := src.HAStatus(context.Background())
	if err != nil || st.Role != RolePrimary || st.SafeMode || !st.LastRoleChange.IsZero() {
		t.Fatalf("live primary = %+v (%v)", st, err)
	}
	if _, err := NewHAMonitorSource(nil).HAStatus(context.Background()); err == nil {
		t.Fatal("a nil monitor must be an error (fail closed)")
	}
}

