package trigger

import "testing"

// fleetpol-01b on a real server: the enrollment trigger of a discovered
// member fires once (key "discovered") whatever how often it is asked.
func TestTriggerDB_EnrolledFiresOnce(t *testing.T) {
	pool, ctx := livePool(t, "enrolled")
	h := NewHub()
	h.Register("cluster-app-3", NewStore(pool), 1<<30)
	detail := map[string]any{"set": "agent-created", "source": "cluster"}
	for range 3 {
		if err := h.Enrolled(ctx, "cluster-app-3", detail); err != nil {
			t.Fatalf("Enrolled: %v", err)
		}
	}
	got, err := NewStore(pool).Pending(ctx, "cluster-app-3")
	if err != nil || len(got) != 1 || got[0].Kind != KindEnrollment ||
		got[0].Key != "discovered" || got[0].Detail["set"] != "agent-created" {
		t.Fatalf("pending = %+v (%v), want one enrollment trigger keyed discovered", got, err)
	}
}
