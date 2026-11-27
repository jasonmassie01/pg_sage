package capacity

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/cost"
)

const orders = "rds:us-east-1/orders"

// seedUsage records 14 days of an idle 8-vCPU instance's hourly usage
// with 100 GiB allocated (the newest hour: 120 GiB, after a resize).
func seedUsage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, now time.Time) {
	t.Helper()
	f := func(v float64) *float64 { return &v }
	last := now.Truncate(time.Hour)
	for i := 0; i < 14*24; i++ {
		alloc := 100 * gib
		if i == 0 {
			alloc = 120 * gib
		}
		u := cloudtel.HourlyUsage{Resource: orders, Hour: last.Add(-time.Duration(i) *
			time.Hour), InstanceClass: "db.r6g.2xlarge", VCPUs: 8,
			MemoryTotalBytes: 64 * gib, AllocatedStorageBytes: alloc, CPUP95: f(20),
			CPUMax: f(35), MemAvailableMin: f(48 * gib), DBLoadP95: f(1), Samples: 60}
		if err := cost.RecordHourly(ctx, pool, u); err != nil {
			t.Fatalf("seed hour %d: %v", i, err)
		}
	}
}

func TestCostRightsizer_LiveVerdictRatesAndNewestAllocation(t *testing.T) {
	pool, ctx := livePool(t, "capacity_rightsize")
	now := time.Now().UTC()
	seedUsage(t, ctx, pool, now)
	price := 1.008
	rates := cost.NewConfigRates(config.CostConfig{Rates: []config.CostRate{{
		Resource: orders, Currency: "USD", AsOf: "2026-11-01", ComputePerHour: &price}}})
	r := CostRightsizer{Pool: pool, Resource: orders, Database: "orders", Rates: rates,
		Now: func() time.Time { return now }}
	rs, err := r.Rightsize(ctx)
	if err != nil {
		t.Fatalf("rightsize: %v", err)
	}
	v := rs.Verdict
	if v.Decision != cost.DecisionDownsize || v.TargetClass != "db.r6g.xlarge" ||
		v.MonthlyDelta == nil || *v.MonthlyDelta >= 0 {
		t.Fatalf("verdict %+v, want a priced downsize to db.r6g.xlarge", v)
	}
	if rs.AllocatedStorageBytes != 120*gib || rs.Resource != orders {
		t.Fatalf("allocated %v of %s, want the newest hour's 120 GiB", rs.AllocatedStorageBytes,
			rs.Resource)
	}
	cands, _, err := New(Config{Rightsizer: r}).Generate(ctx, steadyPack(t), testEnv())
	if err != nil {
		t.Fatal(err)
	}
	got := byKind(cands, KindRightSize)
	if len(got) != 1 || got[0].Prediction.Quantities[QuantityMonthlyDelta] != *v.MonthlyDelta {
		t.Fatalf("right-size candidates %+v", got)
	}
	// 217 GiB projected in 90 days against 120 GiB allocated.
	if len(byKind(cands, KindStorageHeadroom)) != 1 {
		t.Fatalf("no storage headroom advice: %+v", cands)
	}
}

// tzPool is pool's database seen through sessions whose TimeZone is far
// from UTC: the days read and written must not move.
func tzPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["timezone"] = "Pacific/Pago_Pago"
	tz, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tz.Close)
	return tz
}

func TestLive_TheSessionTimeZoneDoesNotMoveTheDays(t *testing.T) {
	pool, ctx := livePool(t, "capacity_tz")
	first := seedDays(t, ctx, pool, 20, 4)
	tz := tzPool(t, ctx, pool)
	days, err := NewDBCallSource(tz).Read(ctx, first, first.AddDate(0, 0, 20))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 20 || QuietDays(days) != 16 || days[3].WorkloadCalls == 0 ||
		days[4].WorkloadCalls != 0 {
		t.Fatalf("days through a -11:00 session: quiet %d, %+v", QuietDays(days), days)
	}
	p, err := Project(series(dbID, "", linear(28, 100*gib, gib, 0)), window.From)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewForecastLog(tz).Record(ctx, "app", p); err != nil {
		t.Fatal(err)
	}
	var made, target string
	if err := pool.QueryRow(ctx, `SELECT made_on::text, target_on::text
		FROM sage.review_capacity_forecast WHERE database_name = 'app'`).Scan(&made,
		&target); err != nil {
		t.Fatal(err)
	}
	if made != "2026-10-31" || target != "2026-11-30" {
		t.Fatalf("stored made_on %s target_on %s, want 2026-10-31 and 2026-11-30", made,
			target)
	}
	got, err := NewForecastLog(tz).Settle(ctx, "app", []DaySize{{
		Day: p.Origin.AddDate(0, 0, ForecastDays), Bytes: 157 * gib}})
	if err != nil || len(got) != 1 || math.Abs(*got[0].ErrorPct) > 1e-9 {
		t.Fatalf("settle through a -11:00 session: %+v, %v", got, err)
	}
}
