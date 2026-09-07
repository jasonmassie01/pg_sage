package forecaster

import "testing"

// No concurrent access tests: aggregation only reads immutable seeded snapshots.
func TestAuditQueryAggsNullableSnapshots(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanupSnapshots(t, pool, ctx, "queries")
	for _, raw := range []string{"null", "[]", `[{"queryid":7,"calls":11}]`} {
		_, err := pool.Exec(ctx, `INSERT INTO sage.snapshots
   (collected_at, category, data) VALUES (now(), 'queries', $1::jsonb)`, raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := QueryDailyQueryAggs(ctx, pool, 1)
	if err != nil {
		t.Fatalf("nullable snapshots must preserve valid query counts: %v", err)
	}
	if len(got) != 1 || got[0].TotalCalls != 11 {
		t.Fatalf("got %#v; want one day with 11 calls", got)
	}
	got, err = QueryDailyQueryAggs(ctx, pool, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("zero lookback got %#v, %v", got, err)
	}
}

func TestAuditSeqAggsNullableSnapshots(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanupSnapshots(t, pool, ctx, "sequences")
	for _, raw := range []string{"null", "[]",
		`[{"schemaname":"public","sequencename":"ids","pct_used":25,"max_value":100}]`} {
		_, err := pool.Exec(ctx, `INSERT INTO sage.snapshots
   (collected_at, category, data) VALUES (now(), 'sequences', $1::jsonb)`, raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := QueryDailySeqAggs(ctx, pool, 1)
	if err != nil {
		t.Fatalf("nullable snapshots must preserve valid sequences: %v", err)
	}
	if len(got) != 1 || got[0].SeqName != "public.ids" || got[0].PctUsed != 25 || got[0].MaxValue != 100 {
		t.Fatalf("unexpected aggregates: %#v", got)
	}
}
