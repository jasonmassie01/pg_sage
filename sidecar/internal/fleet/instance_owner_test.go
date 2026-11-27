package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

// advisors-08: the fleet capability report shows which database manages
// each instance's settings, and why when the identity is unknown.

func TestEnsureCapabilities_ReportsInstanceOwner(t *testing.T) {
	inst := &DatabaseInstance{Name: "beta", InstanceOwner: func() InstanceOwnership {
		return InstanceOwnership{Owner: "alpha", IsOwner: false, KeySource: "system_identifier"}
	}}
	snap := EnsureCapabilities(inst, &InstanceStatus{Platform: "self-managed"})
	got := snap.Capabilities.InstanceOwner
	if got == nil || got.Owner != "alpha" || got.IsOwner || got.KeySource != "system_identifier" {
		t.Fatalf("instance owner = %+v", got)
	}
	raw, err := json.Marshal(snap.Capabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	if !strings.Contains(string(raw), `"instance_owner":{"owner":"alpha","is_owner":false,`+
		`"key_source":"system_identifier"}`) {
		t.Fatalf("capabilities JSON = %s", raw)
	}
}

func TestEnsureCapabilities_UnknownIdentityReason(t *testing.T) {
	inst := &DatabaseInstance{Name: "beta", InstanceOwner: func() InstanceOwnership {
		return InstanceOwnership{IsOwner: true, Reason: "instance identity unreadable"}
	}}
	got := EnsureCapabilities(inst, nil).Capabilities.InstanceOwner
	if got == nil || !got.IsOwner || got.KeySource != "" ||
		got.Reason != "instance identity unreadable" {
		t.Fatalf("instance owner = %+v", got)
	}
}

// Nil: an instance without a registry (single-database mode) reports none.
func TestEnsureCapabilities_NoInstanceOwner(t *testing.T) {
	for _, inst := range []*DatabaseInstance{nil, {Name: "solo"}} {
		if got := EnsureCapabilities(inst, nil).Capabilities.InstanceOwner; got != nil {
			t.Fatalf("instance owner = %+v, want none", got)
		}
	}
}
