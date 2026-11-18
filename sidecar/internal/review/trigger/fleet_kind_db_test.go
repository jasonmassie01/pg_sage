package trigger

import (
	"errors"
	"testing"
)

// The fleet_outlier kind on a real server, and the reads the fleet
// scheduler uses to follow a trigger it fired.

func TestTriggerDB_FleetOutlierFiresAndIsFound(t *testing.T) {
	pool, ctx := livePool(t, "fleet_kind")
	s := NewStore(pool)
	tr := Trigger{Database: "shop", Kind: KindFleetOutlier, Key: "fleet:2026-11",
		Detail: map[string]any{"pass_id": 3, "tokens": 200000}}
	id, fired, err := s.Fire(ctx, tr)
	if err != nil || !fired || id == 0 {
		t.Fatalf("fire fleet_outlier: %d %v %v", id, fired, err)
	}
	got, ok, err := s.Find(ctx, "shop", KindFleetOutlier, "fleet:2026-11")
	if err != nil || !ok || got.ID != id || got.State != StateFired ||
		got.Detail["tokens"] != float64(200000) {
		t.Fatalf("find = %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.Find(ctx, "shop", KindFleetOutlier, "fleet:2026-12"); err != nil || ok {
		t.Fatalf("an unknown key was found (%v)", err)
	}
	byID, err := s.ByID(ctx, id)
	if err != nil || byID.ID != id || byID.Kind != KindFleetOutlier {
		t.Fatalf("by id = %+v %v", byID, err)
	}
	if _, err := s.ByID(ctx, id+1000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing id: %v, want ErrNotFound", err)
	}
	if _, err := (&Store{}).ByID(ctx, id); err == nil {
		t.Fatal("a store without a pool read a trigger")
	}
}
