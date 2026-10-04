package runway

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The disk measurement the WAL-bound credit compares: used bytes are the
// databases plus pg_wal, retained WAL is the largest slot's, and the
// trends are the sampled ones. Without pg_monitor the WAL directory
// cannot be read, so nothing is measured (and nothing is credited).

func seedDiskTrends(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, limit_value)
		SELECT k, 'cluster', 'seed', now() - make_interval(mins => 55 - i * 5),
		       1e9 + i * 1e6, CASE WHEN k = 'disk_used' THEN 1e11 END
		FROM generate_series(0, 11) i, unnest(ARRAY['disk_used', 'database_bytes']) k`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestMeasureDisk_ReadsUsageRetentionAndTrends(t *testing.T) {
	pool, ctx := livePool(t)
	seedDiskTrends(t, ctx, pool)
	runner := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	m, keep := measureDiskWithStableKeep(t, ctx, pool, runner)
	var dbBytes, walBytes float64
	if err := pool.QueryRow(ctx, `SELECT pg_database_size(current_database())::float8,
		(SELECT sum(size)::float8 FROM pg_ls_waldir())`).Scan(&dbBytes, &walBytes); err != nil {
		t.Fatalf("direct: %v", err)
	}
	if m.SlotKeepBytes != keep {
		t.Fatalf("slot keep = %v, want %v", m.SlotKeepBytes, keep)
	}
	if m.UsedBytes < dbBytes+walBytes*0.5 || m.RetainedBytes < 0 {
		t.Fatalf("measure = %+v (db %v wal %v)", m, dbBytes, walBytes)
	}
	if !m.TrendsOK || m.Disk.Samples != 12 || m.Databases.Samples != 12 ||
		m.Disk.Limit != 1e11 {
		t.Fatalf("trends = %+v", m)
	}
	if m.Disk.RatePerS < 3333.3 || m.Disk.RatePerS > 3333.4 {
		t.Fatalf("disk rate = %v, want 1e6 bytes per 5 min", m.Disk.RatePerS)
	}
}

// measureDiskWithStableKeep measures the disk while max_slot_wal_keep_size
// holds still. The setting is cluster-wide and other packages' tests change
// it (ALTER SYSTEM + reload) on the shared CI server, so a measurement is
// only compared with a value read unchanged before and after it.
func measureDiskWithStableKeep(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	runner ProbeRunner) (DiskMeasure, float64) {
	t.Helper()
	for attempt := 1; attempt <= 10; attempt++ {
		before := slotKeepSetting(t, ctx, pool)
		m, err := MeasureDisk(ctx, runner, time.Hour)
		if err != nil {
			t.Fatalf("measure: %v", err)
		}
		if after := slotKeepSetting(t, ctx, pool); after == before {
			return m, before
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("max_slot_wal_keep_size changed during every one of 10 measurements")
	return DiskMeasure{}, 0
}

func slotKeepSetting(t *testing.T, ctx context.Context, pool *pgxpool.Pool) float64 {
	t.Helper()
	var keep float64
	if err := pool.QueryRow(ctx, "SELECT pg_size_bytes("+
		"current_setting('max_slot_wal_keep_size'))::float8").Scan(&keep); err != nil {
		t.Fatalf("read max_slot_wal_keep_size: %v", err)
	}
	return keep
}

func TestMeasureDisk_WithoutPGMonitorMeasuresNothing(t *testing.T) {
	pool, ctx := livePool(t)
	role := fmt.Sprintf("runway_np_%d", os.Getpid())
	ident := pgx.Identifier{role}.Sanitize()
	if _, err := pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident+"; CREATE ROLE "+ident+
		" LOGIN PASSWORD 'runway-test'; GRANT USAGE ON SCHEMA sage TO "+ident+
		"; GRANT SELECT ON sage.runway_samples TO "+ident); err != nil {
		t.Fatalf("role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP OWNED BY "+ident+"; DROP ROLE "+ident)
	})
	u, _ := url.Parse(os.Getenv(testdb.EnvName))
	u.User = url.UserPassword(role, "runway-test")
	restricted, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(restricted.Close)
	_, err = MeasureDisk(ctx, probes.NewRunner(restricted, probes.Catalog(),
		probes.NewLimiter(1)), time.Hour)
	if err == nil || !strings.Contains(err.Error(), "wal_directory") {
		t.Fatalf("measure without pg_monitor = %v, want the WAL directory unreadable", err)
	}
}
