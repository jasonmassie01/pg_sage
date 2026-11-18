//go:build integration

package store

import (
	"fmt"
	"testing"
	"time"
)

// PendingGrades lists every member awaiting its baseline, not the first 50:
// with observe-only members a fleet holds up to 2,050, and 50 members that
// never meet their set's grades must not starve the rest of their grade.
func TestPendingGrades_ListsEveryMemberPastTheFullCap(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	d := NewDiscoveryStore(pool)
	const n = 120
	fillTier(t, ctx, pool, TierObserveOnly, n)
	cands := make([]DiscoveredCandidate, n)
	for i := range cands {
		id := fmt.Sprint(i + 1)
		cands[i] = DiscoveredCandidate{ExternalID: id, Host: "h", Port: 5432, DBName: id,
			SetName: "agent-created"}
	}
	if _, err := d.Upsert(ctx, "cluster", "team", cands, time.Now().UTC()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.discovered_databases d
		SET status = 'enrolled', database_id = m.id
		FROM sage.databases m WHERE m.name = 'fill-observe_only-' || d.external_id`); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	rows, err := d.PendingGrades(ctx)
	if err != nil {
		t.Fatalf("PendingGrades: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("PendingGrades listed %d members, want all %d", len(rows), n)
	}
	if rows[0].ExternalID != "1" || rows[n-1].ExternalID != fmt.Sprint(n) {
		t.Fatalf("PendingGrades order: first %q last %q, want 1 and %d", rows[0].ExternalID,
			rows[n-1].ExternalID, n)
	}
}
