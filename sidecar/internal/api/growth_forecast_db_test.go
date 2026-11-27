package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/forecaster"
)

// forecast-06: GET /api/v1/forecasts/growth is sourced from the runway
// monitor's database_bytes series (sage.runway_samples), fitted with
// regr_slope and regr_r2 over the newest epoch, and carries the same disk
// capacity fields as forecast_disk_growth. It used to read
// sage.size_history, which nothing wrote, so it always returned [].

// No concurrent access tests: the handler reads one query result per
// database into a fresh response; the capacity hook's concurrency is
// covered in the forecaster package.

// seedDatabaseBytes writes an hourly database_bytes series of n points in
// epoch, starting startBytes ten days ago and growing perDay a day.
func seedDatabaseBytes(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	epoch string, n int, startBytes, perDay float64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value)
		SELECT 'database_bytes', 'cluster', $1,
		       now() - interval '10 days' + g * interval '1 hour',
		       $2::float8 + $3::float8 * g / 24.0
		  FROM generate_series(0, $4 - 1) g`,
		epoch, startBytes, perDay, n); err != nil {
		t.Fatalf("seed database_bytes: %v", err)
	}
}

func cleanRunwaySizes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.runway_samples
			WHERE kind IN ('database_bytes', 'disk_used')`)
	}
	clean()
	t.Cleanup(clean)
}

func growthManager(pool *pgxpool.Pool, capacity func(context.Context) forecaster.Capacity,
) *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "alpha", Pool: pool,
		Config: config.DatabaseConfig{Name: "alpha"}, DiskCapacity: capacity})
	return mgr
}

func getGrowth(t *testing.T, mgr *fleet.DatabaseManager, query string) (int, []any) {
	t.Helper()
	w := httptest.NewRecorder()
	growthForecastHandler(mgr).ServeHTTP(w,
		httptest.NewRequest("GET", "/api/v1/forecasts/growth"+query, nil))
	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode growth response: %v", err)
	}
	forecasts, ok := resp["forecasts"].([]any)
	if !ok {
		t.Fatalf("forecasts = %#v, want a list", resp["forecasts"])
	}
	return w.Code, forecasts
}

func declaredCapacity(bytes int64) func(context.Context) forecaster.Capacity {
	return func(ctx context.Context) forecaster.Capacity {
		return forecaster.ResolveCapacity(ctx, bytes, nil)
	}
}

func onlyForecast(t *testing.T, forecasts []any) map[string]any {
	t.Helper()
	if len(forecasts) != 1 {
		t.Fatalf("forecasts = %v, want exactly one", forecasts)
	}
	return forecasts[0].(map[string]any)
}

func TestGrowthForecast_RunwaySamplesGiveSlopeR2AndTimeToFull(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanRunwaySizes(t, ctx, pool)
	// An older epoch grew 50 GB/day: a trend never spans two epochs.
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value) VALUES
		('database_bytes', 'cluster', 'old', now() - interval '20 days', 1e9),
		('database_bytes', 'cluster', 'old', now() - interval '19 days', 51e9),
		('disk_used', 'cluster', 'new', now() - interval '1 hour', 9e12)`); err != nil {
		t.Fatalf("seed the old epoch: %v", err)
	}
	seedDatabaseBytes(t, ctx, pool, "new", 240, 500e9, 6e9)

	code, forecasts := getGrowth(t, growthManager(pool, declaredCapacity(1000e9)),
		"?database=alpha")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	f := onlyForecast(t, forecasts)
	slope, _ := f["growth_rate_bytes_day"].(float64)
	if math.Abs(slope-6e9) > 1e6 {
		t.Fatalf("growth_rate_bytes_day = %v, want about 6e9 (the newest epoch only)", f)
	}
	if r2, _ := f["r2"].(float64); r2 < 0.999 {
		t.Fatalf("r2 = %v, want about 1 for a straight line", f["r2"])
	}
	latest := 500e9 + 6e9*239/24
	if got, _ := f["latest_bytes"].(float64); math.Abs(got-latest) > 1 {
		t.Fatalf("latest_bytes = %v, want %v", f["latest_bytes"], latest)
	}
	if f["data_points"] != float64(240) || f["metric_type"] != "database_bytes" ||
		f["object_name"] != "cluster" || f["database_name"] != "alpha" ||
		f["fleet_database_name"] != "alpha" {
		t.Fatalf("forecast = %v, want 240 points of alpha's cluster database_bytes", f)
	}
	if f["capacity_source"] != forecaster.CapacityDeclared || f["capacity_bytes"] != 1000e9 {
		t.Fatalf("capacity = %v, want declared 1000e9", f)
	}
	want := (1000e9 - latest) / 6e9
	if days, _ := f["days_until_full"].(float64); math.Abs(days-want) > 0.01 {
		t.Fatalf("days_until_full = %v, want %.2f", f["days_until_full"], want)
	}
}

func TestGrowthForecast_UndeclaredAndUnboundedCapacity(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanRunwaySizes(t, ctx, pool)
	seedDatabaseBytes(t, ctx, pool, "e1", 48, 100e9, 2e9)

	_, forecasts := getGrowth(t, growthManager(pool, nil), "")
	f := onlyForecast(t, forecasts)
	if f["capacity_source"] != forecaster.CapacityUndeclared ||
		f["config_key"] != "forecaster.disk_capacity_bytes" {
		t.Fatalf("no capacity hook = %v, want undeclared naming the config key", f)
	}
	if _, set := f["days_until_full"]; set {
		t.Fatalf("undeclared capacity gives days_until_full: %v", f)
	}

	unbounded := func(ctx context.Context) forecaster.Capacity {
		return forecaster.ResolveCapacity(ctx, 1000e9,
			func(context.Context) (float64, string, bool) {
				return 0, forecaster.CapacityUnbounded, false
			})
	}
	_, forecasts = getGrowth(t, growthManager(pool, unbounded), "?database=alpha")
	f = onlyForecast(t, forecasts)
	if f["capacity_source"] != forecaster.CapacityUnbounded ||
		f["authority_under_7_days"] != "managed_storage_runway" {
		t.Fatalf("unbounded provider = %v, want capacity_unbounded, managed authority", f)
	}
	for _, key := range []string{"days_until_full", "capacity_bytes"} {
		if _, set := f[key]; set {
			t.Fatalf("unbounded capacity sets %s (declared used in its place): %v", key, f)
		}
	}
}

func TestGrowthForecast_TooFewSamplesOrNoneGiveEmptyList(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanRunwaySizes(t, ctx, pool)
	mgr := growthManager(pool, declaredCapacity(1000e9))
	if code, forecasts := getGrowth(t, mgr, ""); code != 200 || len(forecasts) != 0 {
		t.Fatalf("no samples: status %d, forecasts %v, want 200 and []", code, forecasts)
	}
	seedDatabaseBytes(t, ctx, pool, "e1", 1, 100e9, 0)
	if _, forecasts := getGrowth(t, mgr, ""); len(forecasts) != 0 {
		t.Fatalf("one sample: forecasts %v, want [] (a slope needs two)", forecasts)
	}
	empty := fleet.NewManager(&config.Config{Mode: "fleet"})
	if code, forecasts := getGrowth(t, empty, "?database=all"); code != 200 ||
		len(forecasts) != 0 {
		t.Fatalf("no databases: status %d, forecasts %v", code, forecasts)
	}
}

func TestGrowthForecast_LookbackWindowAndInvalidInput(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanRunwaySizes(t, ctx, pool)
	seedDatabaseBytes(t, ctx, pool, "e1", 240, 100e9, 1e9)
	mgr := growthManager(pool, declaredCapacity(0))
	// The series starts ten days ago: a 2-day window holds its last 48 hours.
	_, forecasts := getGrowth(t, mgr, "?days=2")
	f := onlyForecast(t, forecasts)
	if pts, _ := f["data_points"].(float64); pts < 40 || pts > 49 {
		t.Fatalf("days=2 data_points = %v, want the last ~48 hourly samples", f["data_points"])
	}
	if _, forecasts := getGrowth(t, mgr, "?days=999"); len(onlyForecast(t, forecasts)) == 0 {
		t.Fatal("days=999 (clamped to 365) returned nothing")
	}
	if code, _ := getGrowth(t, mgr, "?database=bad/name"); code != http.StatusBadRequest {
		t.Fatalf("invalid database: status %d, want 400", code)
	}
	if code, _ := getGrowth(t, mgr, "?database=nope"); code != http.StatusNotFound {
		t.Fatalf("unknown database: status %d, want 404", code)
	}
}

func TestGrowthForecast_QueryErrorIsA500(t *testing.T) {
	_, ctx := phase2RequireDB(t)
	closed, err := pgxpool.New(ctx, p2Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open a second pool: %v", err)
	}
	closed.Close()
	code, _ := getGrowth(t, growthManager(closed, declaredCapacity(0)), "?database=alpha")
	if code != http.StatusInternalServerError {
		t.Fatalf("closed pool: status %d, want 500", code)
	}
}
