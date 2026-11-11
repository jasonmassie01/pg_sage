package recommendation

import (
	"context"
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: LatestByFindingIDs is one read with no
// shared state; the store's own concurrency tests cover its writers.

// LatestByFindingIDs returns the newest head per finding with when it was
// applied, and nothing for a finding without a head (cases-10).
func TestLatestByFindingIDs_NewestHeadPerFinding(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := newApproved(t, ctx, pool, s)
	c := claim(t, ctx, s, rec)
	actionID := insertActionLog(t, ctx, pool, "monitoring", sqlA)
	if err := s.RecordApplied(ctx, c, actionID); err != nil {
		t.Fatalf("record applied: %v", err)
	}
	// An older, superseded head of the same finding must not win.
	var older int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.recommendation
		(identity_key, database_name, category, target, action_type, finding_id, state,
		 revision, content_hash, retry_budget)
		SELECT 'older-' || id, database_name, category, target, action_type, finding_id,
		       'superseded', 1, 'old', 2
		  FROM sage.recommendation WHERE id = $1 RETURNING id`, rec.ID).Scan(&older); err != nil {
		t.Fatalf("seed older head: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.recommendation SET id = $2 WHERE id = $1`,
		older, rec.ID-1000000); err != nil {
		t.Fatalf("make the second head older: %v", err)
	}
	lone := seedFinding(t, ctx, pool, "rec_test_lone_"+uniqueDB(t), "public.lone", sqlA, "")
	heads, err := LatestByFindingIDs(ctx, pool, []int64{*rec.FindingID, lone, -1})
	if err != nil {
		t.Fatalf("LatestByFindingIDs: %v", err)
	}
	got, ok := heads[*rec.FindingID]
	if len(heads) != 1 || !ok {
		t.Fatalf("heads = %+v, want only finding %d", heads, *rec.FindingID)
	}
	if got.ID != rec.ID || got.State != StateApplied || got.ContentHash != rec.ContentHash ||
		got.AttemptCount != 1 || got.AppliedAt == nil {
		t.Fatalf("head = %+v (applied_at %v), want the applied head", got, got.AppliedAt)
	}
	// Compared with the database clock: the server's clock may differ
	// from the test host's.
	var dbNow time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatalf("read database clock: %v", err)
	}
	if age := dbNow.Sub(*got.AppliedAt); age < 0 || age > time.Minute {
		t.Fatalf("applied_at = %v (db now %v), want the transition just recorded",
			got.AppliedAt, dbNow)
	}
}

// A head that was never applied has no applied_at.
func TestLatestByFindingIDs_NeverAppliedHasNoAppliedAt(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	rec := mustPropose(t, ctx, s, proposal(t, ctx, pool, uniqueDB(t), sqlA, inverseA)).
		Recommendation
	heads, err := LatestByFindingIDs(ctx, pool, []int64{*rec.FindingID})
	if err != nil {
		t.Fatalf("LatestByFindingIDs: %v", err)
	}
	if h := heads[*rec.FindingID]; h.ID != rec.ID || h.State != StateProposed ||
		h.AppliedAt != nil {
		t.Fatalf("head = %+v, want proposed and never applied", h)
	}
}

// Nil/empty: no ids reads nothing, even without a database.
func TestLatestByFindingIDs_EmptyReadsNothing(t *testing.T) {
	heads, err := LatestByFindingIDs(context.Background(), nil, nil)
	if err != nil || heads == nil || len(heads) != 0 {
		t.Fatalf("heads, err = %v, %v; want an empty map", heads, err)
	}
}

// Error propagation: a missing database and a failed read are errors
// that say what was read, never an empty answer.
func TestLatestByFindingIDs_Errors(t *testing.T) {
	heads, err := LatestByFindingIDs(context.Background(), nil, []int64{1})
	if err == nil || heads != nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("nil querier: heads, err = %v, %v", heads, err)
	}
	pool, _ := requireDB(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	heads, err = LatestByFindingIDs(cancelled, pool, []int64{1, 2})
	if err == nil || heads != nil ||
		!strings.Contains(err.Error(), "recommendations of 2 findings") {
		t.Fatalf("cancelled read: heads, err = %v, %v", heads, err)
	}
}
