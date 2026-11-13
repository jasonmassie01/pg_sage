package cost

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// Integration: 14 days of recorded host usage on a real server, read back,
// judged against the server's own shared_buffers, and recorded.
func TestEvaluateRightsize_EndToEnd(t *testing.T) {
	pool, ctx := requireDB(t)
	clearUsage(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.findings WHERE category = $1`,
		CategoryRightsize); err != nil {
		t.Fatalf("clear: %v", err)
	}
	now := time.Now().UTC()
	last := now.Truncate(time.Hour)
	for i := 0; i < 14*24; i++ {
		u := cloudtel.HourlyUsage{Resource: "rds:us-east-1/orders",
			Hour: last.Add(-time.Duration(i) * time.Hour), InstanceClass: "db.r6g.2xlarge",
			VCPUs: 8, MemoryTotalBytes: 64 * gb, CPUP95: f64(20), CPUMax: f64(35),
			MemAvailableMin: f64(48 * gb), DBLoadP95: f64(1), Samples: 60}
		if err := RecordHourly(ctx, pool, u); err != nil {
			t.Fatalf("seed hour %d: %v", i, err)
		}
	}
	price := 1.008
	rates := NewConfigRates(config.CostConfig{Rates: []config.CostRate{{
		Resource: "rds:us-east-1/orders", Currency: "USD", AsOf: "2026-11-01",
		ComputePerHour: &price}}})
	v, err := EvaluateRightsize(ctx, pool, "rds:us-east-1/orders", "orders", rates, now)
	if err != nil {
		t.Fatalf("EvaluateRightsize: %v", err)
	}
	if v.Decision != DecisionDownsize || v.TargetClass != "db.r6g.xlarge" {
		t.Fatalf("verdict = %+v, want downsize to db.r6g.xlarge", v)
	}
	var usd float64
	if err := pool.QueryRow(ctx, `SELECT estimated_cost_usd::float8 FROM sage.findings
		WHERE category = $1 AND status = 'open'`, CategoryRightsize).Scan(&usd); err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if usd != -367.92 { // 1.008 x (4/8 - 1) x 730, to the cent
		t.Fatalf("estimated_cost_usd = %v, want -367.92", usd)
	}
}

func TestEvaluateRightsize_NeedsAResource(t *testing.T) {
	pool, ctx := requireDB(t)
	if _, err := EvaluateRightsize(ctx, pool, "", "orders", nil, time.Now()); err == nil {
		t.Fatal("a database without a provider resource must not be judged")
	}
	if _, err := EvaluateRightsize(context.Background(), nil, "x", "orders", nil,
		time.Now()); err == nil {
		t.Fatal("a nil pool must be an error")
	}
}

func TestEvaluateRightsize_NoHistoryIsUnknownAndWritesNothing(t *testing.T) {
	pool, ctx := requireDB(t)
	clearUsage(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.findings WHERE category = $1`,
		CategoryRightsize); err != nil {
		t.Fatalf("clear: %v", err)
	}
	v, err := EvaluateRightsize(ctx, pool, "rds:us-east-1/orders", "orders", nil, time.Now())
	if err != nil || v.Decision != DecisionUnknown {
		t.Fatalf("verdict %+v (%v), want unknown without history", v, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings WHERE category = $1`,
		CategoryRightsize).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d findings (%v), want none", n, err)
	}
}
