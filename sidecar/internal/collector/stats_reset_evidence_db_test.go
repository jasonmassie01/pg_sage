package collector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Statistics-reset evidence is recorded with every snapshot, so unused-
// index evidence can never silently span a reset: the system category
// carries the relation stats epoch (the later of pg_stat_database.
// stats_reset and the postmaster start) and every index its oid.

func resetEvidencePool(t *testing.T) (*pgxpool.Pool, *Collector) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "reset_evidence"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	var version int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).
		Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE public.ev_t (id int PRIMARY KEY, a int);
		CREATE INDEX ev_a ON public.ev_t (a)`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return pool, New(pool, testConfig(), version, noopLog)
}

func liveEpoch(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var epoch time.Time
	if err := pool.QueryRow(context.Background(), `SELECT GREATEST(
		COALESCE(stats_reset, '-infinity'::timestamptz), pg_postmaster_start_time())
		FROM pg_stat_database WHERE datname = current_database()`).Scan(&epoch); err != nil {
		t.Fatalf("live epoch: %v", err)
	}
	return epoch
}

// The recorded epoch is the database's, and it moves on pg_stat_reset()
// and on a single-relation reset (pg_stat_reset_single_table_counters),
// which PostgreSQL 14-18 all record in pg_stat_database.stats_reset.
func TestCollectSystem_RecordsRelationStatsEpoch(t *testing.T) {
	pool, c := resetEvidencePool(t)
	ctx := context.Background()
	s1, err := c.collectSystem(ctx)
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	if s1.RelationStatsEpoch.IsZero() || !s1.RelationStatsEpoch.Equal(liveEpoch(t, pool)) {
		t.Fatalf("epoch = %s, want the live %s", s1.RelationStatsEpoch, liveEpoch(t, pool))
	}
	for _, reset := range []string{`SELECT pg_stat_reset()`,
		`SELECT pg_stat_reset_single_table_counters('public.ev_a'::regclass)`} {
		before := s1.RelationStatsEpoch
		time.Sleep(10 * time.Millisecond)
		if _, err := pool.Exec(ctx, reset); err != nil {
			t.Fatalf("%s: %v", reset, err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for !s1.RelationStatsEpoch.After(before) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond) // PG14's stats collector applies resets async
			if s1, err = c.collectSystem(ctx); err != nil {
				t.Fatalf("collectSystem: %v", err)
			}
		}
		if !s1.RelationStatsEpoch.After(before) {
			t.Fatalf("%s: epoch stayed %s", reset, s1.RelationStatsEpoch)
		}
	}
}

// Every index carries its oid: a dropped and recreated index under the
// same name is a new object with no usage history.
func TestCollectIndexes_RecordsIndexOID(t *testing.T) {
	pool, c := resetEvidencePool(t)
	ctx := context.Background()
	var oid uint32
	if err := pool.QueryRow(ctx, `SELECT 'public.ev_a'::regclass::oid`).Scan(&oid); err != nil {
		t.Fatalf("oid: %v", err)
	}
	indexes, err := c.collectIndexes(ctx)
	if err != nil {
		t.Fatalf("collectIndexes: %v", err)
	}
	found := false
	for _, ix := range indexes {
		if ix.IndexRelName == "ev_a" {
			found = ix.IndexRelID == oid
		}
		if ix.IndexRelID == 0 {
			t.Errorf("%s has no oid", ix.IndexRelName)
		}
	}
	if !found {
		t.Fatalf("ev_a oid not recorded (want %d) in %+v", oid, indexes)
	}
}

// The epoch is persisted with the system snapshot and read back; a legacy
// system document without it reads as unknown (zero), and an unknown epoch
// is not written.
func TestSystemStatsJSON_RelationStatsEpoch(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(SystemStats{RelationStatsEpoch: at, CacheHitRatio: 0.9})
	const want = `"relation_stats_epoch":"2026-10-02T12:00:00Z"`
	if err != nil || !strings.Contains(string(raw), want) {
		t.Fatalf("marshal = %s (%v)", raw, err)
	}
	var back SystemStats
	if err := json.Unmarshal(raw, &back); err != nil || !back.RelationStatsEpoch.Equal(at) {
		t.Fatalf("round trip = %+v (%v)", back, err)
	}
	var legacy SystemStats
	if err := json.Unmarshal([]byte(`{"db_size_bytes":1}`), &legacy); err != nil ||
		!legacy.RelationStatsEpoch.IsZero() {
		t.Fatalf("legacy = %+v (%v), want a zero epoch", legacy, err)
	}
	raw, _ = json.Marshal(SystemStats{})
	if strings.Contains(string(raw), "relation_stats_epoch") {
		t.Fatalf("unknown epoch written: %s", raw)
	}
	raw, _ = json.Marshal(IndexStats{IndexRelID: 42})
	if !strings.Contains(string(raw), `"indexrelid":42`) {
		t.Fatalf("index oid not persisted: %s", raw)
	}
}
