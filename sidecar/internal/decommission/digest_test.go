package decommission

import (
	"encoding/hex"
	"testing"
	"time"
)

func digestState(acked map[string]bool, ids ...string) legacyState {
	st := legacyState{tablesPresent: true, controlDB: "control", acked: acked,
		evidence: map[string][]string{}, liveReceipts: map[string]string{},
		receipts: map[string]creationReceipt{}, schemas: map[string]bool{},
		databases: map[string]bool{}}
	for _, id := range ids {
		st.deployments = append(st.deployments, deployment{ID: id, Provider: "aws_rds",
			LiveMode: true, ResourceID: "res-" + id, Status: "active"})
	}
	return st
}

func digestOf(st legacyState, at time.Time) string {
	return buildInventory(st, func(string) (string, bool) { return "", false }, nil, at).
		InventorySHA256
}

func TestInventoryDigest_CoversItemsNotAcksOrTime(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	base := digestOf(digestState(map[string]bool{}, "d1"), t0)
	if raw, err := hex.DecodeString(base); err != nil || len(raw) != 32 {
		t.Fatalf("digest %q is not hex SHA-256: %v", base, err)
	}
	acked := digestOf(digestState(map[string]bool{"provider_resource:d1": true}, "d1"), t0)
	if acked != base {
		t.Fatal("acknowledging an item changed the digest; export-then-ack would go stale")
	}
	if later := digestOf(digestState(map[string]bool{}, "d1"), t0.Add(time.Hour)); later != base {
		t.Fatal("the generation time changed the digest")
	}
	if more := digestOf(digestState(map[string]bool{}, "d1", "d2"), t0); more == base {
		t.Fatal("a new item did not change the digest")
	}
	moved := digestState(map[string]bool{}, "d1")
	moved.deployments[0].ResourceID = "res-other"
	if digestOf(moved, t0) == base {
		t.Fatal("a changed resource id did not change the digest")
	}
	other := digestState(map[string]bool{}, "d1")
	other.controlDB = "elsewhere"
	if digestOf(other, t0) == base {
		t.Fatal("another control database did not change the digest")
	}
}

func TestInventoryDigest_EmptyInventoryIsStableAndDistinct(t *testing.T) {
	t0 := time.Now()
	empty := digestOf(digestState(map[string]bool{}), t0)
	if empty == "" || empty != digestOf(digestState(map[string]bool{}), t0.Add(time.Minute)) {
		t.Fatalf("empty digest %q is not stable", empty)
	}
	if empty == digestOf(digestState(map[string]bool{}, "d1"), t0) {
		t.Fatal("an empty inventory has the same digest as a one-item inventory")
	}
}
