package notify

import (
	"context"
	"testing"
)

// alerting-06b review: the legacy cutover reads a bounded batch, so open
// pages must not crowd a resolve out of it nor end the sweep early, and a
// reopened finding's newer trigger is owed its own resolve.

func TestLegacyCutoverDB_OpenPagesNeverCrowdOutAResolve(t *testing.T) {
	pool, d, ch, pd := cutoverRig(t, 202, 202)
	prev := legacyPageBatch
	legacyPageBatch = 1
	t.Cleanup(func() { legacyPageBatch = prev })
	ctx := context.Background()
	seedLegacyPage(t, pool) // lower id, stays open
	resolvedID, _ := seedLegacyPage(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved',
		resolved_at = now() WHERE id = $1`, resolvedID); err != nil {
		t.Fatal(err)
	}
	res, err := ResolveLegacyPages(ctx, pool, "db1", d, ch)
	if err != nil || res != (LegacyCutoverResult{Resolved: 1, More: true}) {
		t.Fatalf("first sweep = %+v, %v; want the resolved page first and more to read",
			res, err)
	}
	if hits, _ := pd.counts(); hits != 1 {
		t.Fatalf("pagerduty hits = %d, want the one resolve", hits)
	}
	res, err = ResolveLegacyPages(ctx, pool, "db1", d, ch)
	if err != nil || res != (LegacyCutoverResult{StillOpen: 1, More: true}) {
		t.Fatalf("second sweep = %+v, %v; want the open page still owed", res, err)
	}
}

func TestLegacyCutoverDB_ReopenedFindingIsResolvedAgain(t *testing.T) {
	pool, d, ch, pd := cutoverRig(t, 202)
	id, key := seedLegacyPage(t, pool)
	ctx := context.Background()
	// The legacy engine resolved the first episode, then the finding
	// reopened under the same id and paged again.
	for _, status := range []string{"resolved", "sent"} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.alert_log
			(finding_id, severity, channel, dedup_key, status)
			VALUES ($1, 'critical', 'pagerduty', $2, $3)`, id, key, status); err != nil {
			t.Fatalf("insert %s row: %v", status, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved',
		resolved_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	res, err := ResolveLegacyPages(ctx, pool, "db1", d, ch)
	if err != nil || res != (LegacyCutoverResult{Resolved: 1}) {
		t.Fatalf("sweep = %+v, %v; want the second trigger resolved", res, err)
	}
	if hits, _ := pd.counts(); hits != 1 {
		t.Fatalf("pagerduty hits = %d, want one resolve", hits)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sage.alert_log WHERE finding_id = $1
		AND channel = 'pagerduty' AND status = 'resolved'`, id); n != 2 {
		t.Fatalf("resolved rows = %d, want both episodes resolved", n)
	}
}
