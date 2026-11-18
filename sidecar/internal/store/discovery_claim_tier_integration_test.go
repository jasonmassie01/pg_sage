//go:build integration

package store

import (
	"errors"
	"testing"
	"time"
)

// fleetpol-01d: a set's claim counts the live discovered members of the
// tier it enrolls at, so observe-only members never use up max_enrolled
// and full members never use up the observe-only room.
func TestClaimSet_CountsTheLiveMembersOfItsTier(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	ds, d := NewDatabaseStore(pool, testKey), NewDiscoveryStore(pool)
	now := time.Now().UTC()
	cands := []DiscoveredCandidate{
		{ExternalID: "obs", Host: "h", Port: 5432, DBName: "obs", SetName: "agent-created"},
		{ExternalID: "a2", Host: "h", Port: 5432, DBName: "a2", SetName: "agent-created"},
		{ExternalID: "p1", Host: "h", Port: 5432, DBName: "p1", SetName: "default"},
	}
	if _, err := d.Upsert(ctx, "cluster", "team", cands, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	obsID, err := ds.CreateObserveOnly(ctx, tierInput("obs"), 1)
	if err != nil {
		t.Fatalf("create observe-only member: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.discovered_databases
		SET status = 'enrolled', database_id = $1 WHERE external_id = 'obs'`,
		obsID); err != nil {
		t.Fatalf("enroll obs: %v", err)
	}
	// One observe-only member is live: a full claim with room for one
	// still claims, an observe-only claim with room for one does not.
	got, err := d.ClaimSet(ctx, "cluster", "agent-created", TierObserveOnly, 5, 1, now)
	if err != nil || len(got) != 0 {
		t.Fatalf("observe-only claim at its max: %+v (%v), want none", got, err)
	}
	got, err = d.ClaimSet(ctx, "cluster", "default", TierFull, 5, 1, now)
	if err != nil || len(got) != 1 || got[0].ExternalID != "p1" {
		t.Fatalf("full claim next to an observe-only member: %+v (%v), want p1", got, err)
	}
	// p1 is enrolling with no member yet: it counts against both tiers.
	got, err = d.ClaimSet(ctx, "cluster", "agent-created", TierObserveOnly, 5, 2, now)
	if err != nil || len(got) != 0 {
		t.Fatalf("observe-only claim with an enrolling row: %+v (%v), want none", got, err)
	}
	got, err = d.ClaimSet(ctx, "cluster", "agent-created", TierObserveOnly, 5, 3, now)
	if err != nil || len(got) != 1 || got[0].ExternalID != "a2" {
		t.Fatalf("observe-only claim with room: %+v (%v), want a2", got, err)
	}
	if _, err := d.ClaimSet(ctx, "cluster", "default", "light", 5, 3, now); !errors.Is(err,
		ErrValidation) {
		t.Fatalf("tier light: %v, want ErrValidation", err)
	}
}
