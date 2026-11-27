package cost

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/schema"
)

func clearUsage(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.host_usage_hourly`); err != nil {
		t.Fatalf("clear: %v", err)
	}
}

func usageAt(hour time.Time, samples int, class string, cpu float64) cloudtel.HourlyUsage {
	return cloudtel.HourlyUsage{Resource: "rds:us-east-1/orders", Hour: hour,
		InstanceClass: class, VCPUs: 8, MemoryTotalBytes: 64 * gb,
		AllocatedStorageBytes: 100 * gb, CPUP50: f64(cpu / 2), CPUP95: f64(cpu),
		CPUMax: f64(cpu + 5), MemAvailableMin: f64(40 * gb), DBLoadP95: f64(1.5),
		ConnectionsMax: f64(30), Samples: samples}
}

func TestBootstrapTwiceKeepsTheUsageTable(t *testing.T) {
	pool, ctx := requireDB(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_indexes
		WHERE schemaname = 'sage' AND tablename = 'host_usage_hourly'`).Scan(&n); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	if n < 1 {
		t.Fatalf("host_usage_hourly has %d indexes, want its primary key", n)
	}
}

func TestRecordHourly_UpsertsOneRowPerResourceHour(t *testing.T) {
	pool, ctx := requireDB(t)
	clearUsage(t, ctx, pool)
	hour := time.Now().UTC().Truncate(time.Hour)
	for _, u := range []cloudtel.HourlyUsage{usageAt(hour, 1, "db.r6g.2xlarge", 10),
		usageAt(hour, 5, "db.r6g.2xlarge", 30)} {
		if err := RecordHourly(ctx, pool, u); err != nil {
			t.Fatalf("RecordHourly: %v", err)
		}
	}
	// A restarted process's smaller aggregate does not replace a fuller one.
	if err := RecordHourly(ctx, pool, usageAt(hour, 2, "db.r6g.2xlarge", 99)); err != nil {
		t.Fatalf("RecordHourly: %v", err)
	}
	rows, err := LoadHours(ctx, pool, "rds:us-east-1/orders", hour.Add(-time.Hour))
	if err != nil {
		t.Fatalf("LoadHours: %v", err)
	}
	if len(rows) != 1 || rows[0].Samples != 5 || rows[0].CPUP95 == nil ||
		*rows[0].CPUP95 != 30 || rows[0].InstanceClass != "db.r6g.2xlarge" ||
		rows[0].VCPUs != 8 || rows[0].AllocatedStorageBytes != 100*gb {
		t.Fatalf("rows = %+v, want one 5-sample row at 30%%", rows)
	}
	// A resize mid-hour replaces the row whatever its sample count.
	if err := RecordHourly(ctx, pool, usageAt(hour, 1, "db.r6g.xlarge", 70)); err != nil {
		t.Fatalf("RecordHourly: %v", err)
	}
	rows, _ = LoadHours(ctx, pool, "rds:us-east-1/orders", hour.Add(-time.Hour))
	if len(rows) != 1 || rows[0].InstanceClass != "db.r6g.xlarge" || rows[0].Samples != 1 {
		t.Fatalf("after resize rows = %+v, want the new class's row", rows)
	}
}

// Concurrent access: two fleet databases writing the same resource hour
// into one sage schema leave one row.
func TestRecordHourly_ConcurrentWritersOneRow(t *testing.T) {
	pool, ctx := requireDB(t)
	clearUsage(t, ctx, pool)
	hour := time.Now().UTC().Truncate(time.Hour)
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := RecordHourly(ctx, pool, usageAt(hour, n, "db.r6g.2xlarge",
				float64(n))); err != nil {
				t.Errorf("RecordHourly: %v", err)
			}
		}(i)
	}
	wg.Wait()
	var n, samples int
	if err := pool.QueryRow(ctx, `SELECT count(*), max(samples) FROM sage.host_usage_hourly
		WHERE resource = 'rds:us-east-1/orders'`).Scan(&n, &samples); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 || samples != 8 {
		t.Fatalf("%d rows (max samples %d), want 1 row with the fullest aggregate", n, samples)
	}
}

func TestRecordHourly_RetentionDropsRowsOlderThan35Days(t *testing.T) {
	pool, ctx := requireDB(t)
	clearUsage(t, ctx, pool)
	now := time.Now().UTC().Truncate(time.Hour)
	old := usageAt(now.Add(-36*24*time.Hour), 60, "db.r6g.2xlarge", 10)
	keep := usageAt(now.Add(-34*24*time.Hour), 60, "db.r6g.2xlarge", 10)
	for _, u := range []cloudtel.HourlyUsage{old, keep} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.host_usage_hourly
			(resource, hour, samples) VALUES ($1, $2, $3)`, u.Resource, u.Hour,
			u.Samples); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := RecordHourly(ctx, pool, usageAt(now, 1, "db.r6g.2xlarge", 10)); err != nil {
		t.Fatalf("RecordHourly: %v", err)
	}
	rows, err := LoadHours(ctx, pool, "rds:us-east-1/orders", now.Add(-40*24*time.Hour))
	if err != nil {
		t.Fatalf("LoadHours: %v", err)
	}
	if len(rows) != 2 || !rows[0].Hour.Equal(keep.Hour) {
		t.Fatalf("rows = %+v, want the 34-day row and the new one", rows)
	}
}

func TestRecordHourly_InvalidInputRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	if err := RecordHourly(ctx, pool, cloudtel.HourlyUsage{Hour: time.Now()}); err == nil {
		t.Fatal("a row without a resource must be refused")
	}
	if err := RecordHourly(ctx, pool, cloudtel.HourlyUsage{Resource: "x"}); err == nil {
		t.Fatal("a row without an hour must be refused")
	}
}

func TestApplyRightsize_WritesTheAdvisoryFindingWithItsDollarDelta(t *testing.T) {
	pool, ctx := requireDB(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.findings WHERE category = $1`,
		CategoryRightsize); err != nil {
		t.Fatalf("clear: %v", err)
	}
	v := verdictFor(hoursOf(336, r6g2x()), 8*gb, usdRates())
	if err := ApplyRightsize(ctx, pool, "rds:us-east-1/orders", v); err != nil {
		t.Fatalf("ApplyRightsize: %v", err)
	}
	var usd *float64
	var sql *string
	var status, severity string
	if err := pool.QueryRow(ctx, `SELECT estimated_cost_usd::float8, recommended_sql,
		status, severity FROM sage.findings WHERE category = $1`,
		CategoryRightsize).Scan(&usd, &sql, &status, &severity); err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if usd == nil || *usd != -365 || (sql != nil && *sql != "") || status != "open" ||
		severity != "info" {
		t.Fatalf("finding usd=%v sql=%v status=%s severity=%s", usd, sql, status, severity)
	}
	// CHECK-02: only instance_rightsize carries a dollar figure.
	var others int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings
		WHERE estimated_cost_usd IS NOT NULL AND category <> $1`,
		CategoryRightsize).Scan(&others); err != nil {
		t.Fatalf("count: %v", err)
	}
	if others != 0 {
		t.Fatalf("%d other findings carry estimated_cost_usd", others)
	}
	// State transition: a keep verdict resolves it; unknown leaves it.
	if err := ApplyRightsize(ctx, pool, "rds:us-east-1/orders",
		Verdict{Decision: DecisionKeep}); err != nil {
		t.Fatalf("ApplyRightsize keep: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM sage.findings WHERE category = $1`,
		CategoryRightsize).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("after keep: status %q (%v), want resolved", status, err)
	}
}

func TestApplyRightsize_UnknownLeavesOpenFindings(t *testing.T) {
	pool, ctx := requireDB(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.findings WHERE category = $1`,
		CategoryRightsize); err != nil {
		t.Fatalf("clear: %v", err)
	}
	v := verdictFor(hoursOf(336, r6g2x()), 8*gb, nil)
	if err := ApplyRightsize(ctx, pool, "rds:us-east-1/orders", v); err != nil {
		t.Fatalf("ApplyRightsize: %v", err)
	}
	if err := ApplyRightsize(ctx, pool, "rds:us-east-1/orders",
		Verdict{Decision: DecisionUnknown, Reason: "stale"}); err != nil {
		t.Fatalf("ApplyRightsize unknown: %v", err)
	}
	var status string
	var usd *float64
	if err := pool.QueryRow(ctx, `SELECT status, estimated_cost_usd::float8
		FROM sage.findings WHERE category = $1`, CategoryRightsize).Scan(&status,
		&usd); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "open" || usd != nil {
		t.Fatalf("status %s usd %v: unknown keeps the finding; no rates, no dollars",
			status, usd)
	}
}
