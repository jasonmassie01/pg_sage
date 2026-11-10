package autoexplain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// collection-09: captures of one query on two days scan two daily
// partitions; both are stored with one resolved (v2) plan_hash.

func explainDay(t *testing.T, ctx context.Context, pool *pgxpool.Pool, day string) []byte {
	t.Helper()
	var plan []byte
	if err := pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) SELECT id FROM aex_daily.events
		WHERE ts = DATE '`+day+`'`).Scan(&plan); err != nil {
		t.Fatalf("explain %s: %v", day, err)
	}
	if !strings.Contains(string(plan), "events_p"+strings.ReplaceAll(day, "-", "")) {
		t.Fatalf("plan for %s does not scan its partition: %s", day, plan)
	}
	return plan
}

func dailyEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	drop := "DROP SCHEMA IF EXISTS aex_daily CASCADE"
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), drop) })
	for _, s := range []string{drop, "CREATE SCHEMA aex_daily",
		`CREATE TABLE aex_daily.events (id int, ts date NOT NULL) PARTITION BY RANGE (ts)`,
		`CREATE TABLE aex_daily.events_p20261105 PARTITION OF aex_daily.events
			FOR VALUES FROM ('2026-11-05') TO ('2026-11-06')`,
		`CREATE TABLE aex_daily.events_p20261106 PARTITION OF aex_daily.events
			FOR VALUES FROM ('2026-11-06') TO ('2026-11-07')`} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestStorePlan_DailyPartitionCapturesShareOneHash(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dailyEvents(t, ctx, pool)
	const qid = int64(999999921)
	cleanExplainRows(t, pool, qid)
	t.Cleanup(func() { cleanExplainRows(t, pool, qid) })

	var warnings []string
	c := NewCollector(pool, CollectorConfig{CollectIntervalSeconds: 60},
		&Availability{Method: "unavailable"}, func(level, f string, a ...any) {
			warnings = append(warnings, level+" "+f)
		})
	for _, day := range []string{"2026-11-05", "2026-11-06"} {
		if err := c.storePlan(ctx, qid, "SELECT id FROM aex_daily.events WHERE ts = $1",
			explainDay(t, ctx, pool, day), "on_demand", 1, 0); err != nil {
			t.Fatalf("storePlan %s: %v", day, err)
		}
	}
	var distinct int
	var hash string
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT plan_hash), min(plan_hash)
		FROM sage.explain_cache WHERE queryid = $1`, qid).Scan(&distinct, &hash); err != nil {
		t.Fatalf("read hashes: %v", err)
	}
	if distinct != 1 || !strings.HasPrefix(hash, "v2:") {
		t.Fatalf("two daily captures stored %d distinct hashes (%s), want one v2 hash",
			distinct, hash)
	}
	if len(warnings) != 0 {
		t.Fatalf("resolved captures logged %v", warnings)
	}
}

// A capture whose roots cannot be read still gets a fingerprint, the
// unresolved v2u one, and the read error is returned for the caller to
// log; a plan that cannot be fingerprinted gets none and no error.
func TestPlanHashParam_UnreadableRootsGiveUnresolvedHash(t *testing.T) {
	pool := acquireTestPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h, resolveErr := planHashParam(ctx, pool, []byte(hashablePlan))
	if h == nil || !strings.HasPrefix(*h, "v2u:") {
		t.Fatalf("hash = %v, want a v2u fingerprint", h)
	}
	if resolveErr == nil || !strings.Contains(resolveErr.Error(), "partition roots") {
		t.Fatalf("resolve error = %v, want the failed root read", resolveErr)
	}
	h, resolveErr = planHashParam(context.Background(), pool,
		[]byte(`[{"Plan": {"Total Cost": 1.0}}]`))
	if h != nil || resolveErr != nil {
		t.Fatalf("unhashable plan: hash %v err %v, want nil and nil", h, resolveErr)
	}
}
