package verify

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insertLadderQueueRow inserts a finding and a pending queue row with a
// NULL ladder (a row from before the migration looks the same).
func insertLadderQueueRow(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx := t.Context()
	var findingID, queueID int
	ident := fmt.Sprintf("public.ladder_%d", time.Now().UnixNano())
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status)
		VALUES ('unused_index', 'info', 'index', $1, 'ladder', '{}'::jsonb, 'open')
		RETURNING id`, ident).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_queue (finding_id,
		proposed_sql, rollback_sql, action_risk, status)
		VALUES ($1, 'DROP INDEX CONCURRENTLY public.ladder_ix', '', 'moderate', 'pending')
		RETURNING id`, findingID).Scan(&queueID); err != nil {
		t.Fatalf("insert queue row: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_queue WHERE id = $1", queueID)
		_, _ = pool.Exec(bg, "DELETE FROM sage.findings WHERE id = $1", findingID)
	})
	return queueID
}

func TestLadder_NullBeforeAnyRecord(t *testing.T) {
	pool := verifyIntegrationPool(t)
	id := insertLadderQueueRow(t, pool)
	raw, err := ReadLadder(t.Context(), pool, id)
	if err != nil {
		t.Fatalf("ReadLadder: %v", err)
	}
	if raw != nil {
		t.Fatalf("ladder = %s, want NULL for a row nothing recorded on", raw)
	}
	if _, err := ReadLadder(t.Context(), pool, -1); err == nil {
		t.Fatal("ReadLadder of a missing queue row returned no error")
	}
}

// State transitions: queued (tier 1), rehearsed (tier 2), executed (tier 3
// at the verdict) give three records in that order.
func TestLadder_AppendsInOrder(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertLadderQueueRow(t, pool)
	steps := []LadderRecord{
		FromDropMeasurement(WhatIfEvidence{Verdict: "verified", ChangePct: ladderPct(0.2)},
			ladderNow),
		FromRehearsal(Rehearsal{Passed: true, ReceiptID: 812,
			BlocksPerCallPct: ladderPct(-41)}, ladderNow.Add(time.Hour)),
		FromVerdict(OutcomeImproved, ladderPct(-35), 9, ladderNow.Add(2*time.Hour)),
	}
	for _, rec := range steps {
		if err := AppendLadder(ctx, pool, id, rec); err != nil {
			t.Fatalf("AppendLadder: %v", err)
		}
	}
	raw, err := ReadLadder(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLadder(raw)
	if err != nil {
		t.Fatalf("ParseLadder(%s): %v", raw, err)
	}
	if len(got) != 3 {
		t.Fatalf("ladder = %+v, want three records", got)
	}
	for i, tier := range []int{TierPlanOnly, TierClone, TierCanary} {
		if got[i].Tier != tier || !got[i].Passed() {
			t.Errorf("record %d = %+v, want a passed tier %d", i, got[i], tier)
		}
	}
	if got[1].Evidence["receipt_id"] != float64(812) {
		t.Fatalf("rehearsal evidence = %v", got[1].Evidence)
	}
}

func TestLadder_AppendToMissingRowFails(t *testing.T) {
	pool := verifyIntegrationPool(t)
	err := AppendLadder(t.Context(), pool, -1, FromRehearsal(Rehearsal{Passed: true},
		ladderNow))
	if err == nil {
		t.Fatal("AppendLadder to a missing queue row returned no error")
	}
	if err := AppendLadder(t.Context(), pool, 1); err != nil {
		t.Fatalf("AppendLadder with no records = %v, want a no-op", err)
	}
}

// Concurrency: two producers appending to one row at once both land.
func TestLadder_ConcurrentAppendsBothLand(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertLadderQueueRow(t, pool)
	const per = 10
	var wg sync.WaitGroup
	errs := make(chan error, 2*per)
	for p := 0; p < 2; p++ {
		wg.Add(1)
		go func(producer int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				rec := FromRehearsal(Rehearsal{Passed: true,
					ReceiptID: int64(producer*1000 + i + 1)}, ladderNow)
				errs <- AppendLadder(ctx, pool, id, rec)
			}
		}(p)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AppendLadder: %v", err)
		}
	}
	raw, err := ReadLadder(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLadder(raw)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[float64]bool{}
	for _, r := range got {
		seen[r.Evidence["receipt_id"].(float64)] = true
	}
	if len(got) != 2*per || len(seen) != 2*per {
		t.Fatalf("ladder has %d records (%d distinct), want %d: an append was lost",
			len(got), len(seen), 2*per)
	}
}

// createLadderSortTable creates and analyzes the 20000-row table the
// session-setting tests plan sorts on.
func createLadderSortTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.ladder_sort AS
		SELECT g AS id, md5(g::text) AS v FROM generate_series(1, 20000) g`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.ladder_sort")
	})
	if _, err := pool.Exec(ctx, "ANALYZE public.ladder_sort"); err != nil {
		t.Fatal(err)
	}
}

// Happy path: a work_mem step's plans are read under SET LOCAL in a
// transaction that is rolled back, so the session's work_mem is unchanged.
func TestFromSessionSetting_WorkMemRollsBack(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	createLadderSortTable(t, pool)
	before := showWorkMem(t, conn.Conn())
	r := FromSessionSetting(ctx, conn.Conn(), SessionSetting{Name: "work_mem",
		Value: "256MB", Queries: []string{
			"SELECT v FROM public.ladder_sort ORDER BY v"}}, ladderNow)
	if after := showWorkMem(t, conn.Conn()); after != before {
		t.Fatalf("work_mem = %q after the test, was %q: the transaction did not roll back",
			after, before)
	}
	if r.Tier != TierPlanOnly || r.Method != MethodLadderSession {
		t.Fatalf("record = %+v, want tier 1 session_setting", r)
	}
	if r.Result != LadderPassed || r.Predicted == nil {
		t.Fatalf("record = %+v, want a passed record with a predicted cost change", r)
	}
	if r.Evidence["planned"] != 1 || r.Evidence["setting"] != "work_mem=256MB" {
		t.Fatalf("evidence = %v, want one planned query and the setting", r.Evidence)
	}
	if len(r.Skipped) != 0 || !r.Passed() {
		t.Fatalf("record = %+v, want a pass with nothing skipped", r)
	}
	// A target that is not a read is not planned: the test is partial, so
	// it is inconclusive (never a pass), and the DELETE never runs.
	partial := FromSessionSetting(ctx, conn.Conn(), SessionSetting{Name: "work_mem",
		Value: "256MB", Queries: []string{"SELECT v FROM public.ladder_sort ORDER BY v",
			"DELETE FROM public.ladder_sort"}}, ladderNow)
	if partial.Passed() || partial.Result != LadderInconclusive ||
		len(partial.Skipped) != 1 || partial.Skipped[0].Tier != TierPlanOnly {
		t.Fatalf("partial = %+v, want inconclusive with the DELETE skipped", partial)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM public.ladder_sort").
		Scan(&n); err != nil || n != 20000 {
		t.Fatalf("rows = %d (%v): the probe changed the table", n, err)
	}
}

// Error propagation: a query that fails to plan is recorded as skipped
// with the error, and the transaction is still rolled back.
func TestFromSessionSetting_PlanErrorIsSkipped(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	before := showWorkMem(t, conn.Conn())
	r := FromSessionSetting(ctx, conn.Conn(), SessionSetting{Name: "work_mem",
		Value: "128MB", Queries: []string{"SELECT * FROM public.ladder_no_such_table"}},
		ladderNow)
	if r.Passed() || r.Result != LadderSkipped {
		t.Fatalf("record = %+v, want skipped", r)
	}
	if len(r.Skipped) == 0 || r.Skipped[0].Reason == "" {
		t.Fatalf("skipped = %+v, want the plan error", r.Skipped)
	}
	if after := showWorkMem(t, conn.Conn()); after != before {
		t.Fatalf("work_mem = %q, was %q", after, before)
	}
	bad := FromSessionSetting(ctx, conn.Conn(), SessionSetting{Name: "work_mem",
		Value: "not a size", Queries: []string{"SELECT 1"}}, ladderNow)
	if bad.Passed() || len(bad.Skipped) != 1 {
		t.Fatalf("bad value = %+v, want skipped", bad)
	}
}

func showWorkMem(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var v string
	if err := conn.QueryRow(t.Context(), "SHOW work_mem").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
