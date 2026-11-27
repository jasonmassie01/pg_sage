package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/value"
)

// market-04: GET /api/v1/value carries the cost section.

func getValueCost(t *testing.T, router http.Handler, path string) cost.Section {
	t.Helper()
	w := get(t, router, path)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body=%s", path, w.Code, w.Body.String())
	}
	var body struct {
		Cost *cost.Section `json:"cost"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Cost == nil {
		t.Fatalf("no cost section in %s", w.Body.String())
	}
	return *body.Cost
}

func costRouter(pools map[string]*pgxpool.Pool, rates ...config.CostRate) http.Handler {
	cfg := &config.Config{Mode: "fleet", Cost: config.CostConfig{Rates: rates}}
	mgr := fleet.NewManager(cfg)
	for name, pool := range pools {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: true, LastSeen: time.Now()}})
	}
	return NewRouter(mgr, cfg, nil, fakeAdminMiddleware)
}

func usdRate(resource, database string, perHour float64) config.CostRate {
	return config.CostRate{Resource: resource, Database: database, Currency: "USD",
		AsOf: "2026-11-01", ComputePerHour: &perHour}
}

func seedStorePair(t *testing.T, pool *pgxpool.Pool, qid int64, deltaMs float64) {
	t.Helper()
	ctx := context.Background()
	epoch := time.Now().Add(-48 * time.Hour)
	for i, total := range []float64{1000, 1000 + deltaMs} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time, stats_epoch)
			VALUES (now() - $1::interval, $2, $3, $4, 1, $5)`,
			time.Duration(2-i)*time.Hour, qid, 10+i, total, epoch); err != nil {
			t.Fatalf("seed query_store: %v", err)
		}
	}
}

// CHECK-01: with config rates set, the section's rate_source is "config"
// and as_of is set.
func TestValueCost_ConfiguredRatesPriceTheQueries(t *testing.T) {
	pools := valueFleetPools(t, "costa")
	seedStorePair(t, pools["costa"], 4242, 30_000)
	seedStorePair(t, pools["costa"], 4343, 10_000)
	router := costRouter(pools, usdRate("", "costa", 0.5))
	s := getValueCost(t, router, "/api/v1/value")
	if !s.Available || s.RateSource != "config" || s.AsOf != "2026-11-01" ||
		s.Currency != "USD" {
		t.Fatalf("section = %+v, want config rates as of 2026-11-01", s)
	}
	if s.Compute == nil || *s.Compute < 0.5*167 || *s.Compute > 0.5*169 {
		t.Fatalf("compute = %v, want ~$84 for the default 7-day window", s.Compute)
	}
	if len(s.TopQueries) != 2 || s.TopQueries[0].QueryID != "4242" ||
		s.TopQueries[0].Database != "costa" || s.TopQueries[0].Share != 0.75 {
		t.Fatalf("top = %+v, want 4242 first with 75%%", s.TopQueries)
	}
}

func TestValueCost_NoRatesShowsNoDollars(t *testing.T) {
	pools := valueFleetPools(t, "costb")
	seedStorePair(t, pools["costb"], 77, 5000)
	s := getValueCost(t, costRouter(pools), "/api/v1/value")
	if s.Available || s.Reason != cost.ReasonNotConfigured || s.Total != nil ||
		s.Compute != nil || len(s.TopQueries) != 0 {
		t.Fatalf("section = %+v, want rates not configured and no figures", s)
	}
}

type fixedSource struct{ sample cloudtel.Sample }

func (f fixedSource) Provider() string { return "rds" }
func (f fixedSource) Collect(context.Context, time.Time) (cloudtel.Sample, error) {
	return f.sample, nil
}

// Fleet: two databases on one instance price the instance once.
func TestValueCost_SharedInstanceCountedOnce(t *testing.T) {
	pools := valueFleetPools(t, "costc", "costd")
	rt, err := cloudtel.NewRuntime(fixedSource{cloudtel.Sample{Provider: "rds",
		Resource: "rds:us-east-1/shared"}}, cloudtel.RuntimeOptions{Database: "costc",
		Limits: cloudtel.DefaultLimits()})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if err := rt.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	for _, db := range []string{"costc", "costd"} {
		cloudtel.RegisterMember(db, rt)
		t.Cleanup(func() { cloudtel.UnregisterMember(db, rt) })
	}
	router := costRouter(pools, usdRate("rds:us-east-1/shared", "", 1))
	s := getValueCost(t, router, "/api/v1/value?since=2026-11-01&until=2026-11-01")
	if !s.Available || len(s.Resources) != 1 || len(s.Resources[0].Databases) != 2 ||
		s.Compute == nil || *s.Compute != 24 {
		t.Fatalf("section = %+v, want one resource at $24 for one day", s)
	}
	one := getValueCost(t, router, "/api/v1/value?database=costd&since=2026-11-01"+
		"&until=2026-11-01")
	if len(one.Resources) != 1 || len(one.Resources[0].Databases) != 1 {
		t.Fatalf("?database=costd resources = %+v", one.Resources)
	}
}

func TestCostWindow(t *testing.T) {
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	from, to := costWindow(value.Filter{}, now)
	if !to.Equal(now) || to.Sub(from) != 7*24*time.Hour {
		t.Fatalf("default window %v..%v, want the last 7 days", from, to)
	}
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	from, to = costWindow(value.Filter{Since: day, Until: day.AddDate(0, 0, 1)}, now)
	if !from.Equal(day) || to.Sub(from) != 24*time.Hour {
		t.Fatalf("one-day window %v..%v", from, to)
	}
	from, to = costWindow(value.Filter{Until: now.Add(48 * time.Hour)}, now)
	if !to.Equal(now) {
		t.Fatalf("a future until must stop at now, got %v", to)
	}
	from, _ = costWindow(value.Filter{Since: now.AddDate(-1, 0, 0)}, now)
	if now.Sub(from) != cost.UsageRetention {
		t.Fatalf("a year-long window must be clipped to 35 days, got %v", now.Sub(from))
	}
	from, to = costWindow(value.Filter{Since: now.Add(time.Hour)}, now)
	if from.After(to) {
		t.Fatalf("since after now: %v..%v, want an empty window", from, to)
	}
}

// A value reader without cost attribution (tests, other servers) reports
// the section unavailable rather than inventing figures.
func TestValueCost_UnwiredReaderIsUnavailable(t *testing.T) {
	reader := valueReaderFunc(func(context.Context, value.Filter) (value.Report, error) {
		return value.Report{}, nil
	})
	rec := httptest.NewRecorder()
	valueHandler(reader).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/value", nil))
	var body struct {
		Cost cost.Section `json:"cost"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Cost.Available || body.Cost.Reason != reasonCostUnwired {
		t.Fatalf("cost = %+v, want unavailable", body.Cost)
	}
}

type valueReaderFunc func(context.Context, value.Filter) (value.Report, error)

func (f valueReaderFunc) Get(ctx context.Context, filter value.Filter) (value.Report,
	error) {
	return f(ctx, filter)
}

// Fleet: two entries for one physical database price it and its queries
// once, as the value ledger dedupes them.
func TestValueCost_SamePhysicalDatabaseCountedOnce(t *testing.T) {
	pools := valueFleetPools(t, "coste")
	seedStorePair(t, pools["coste"], 5151, 20_000)
	pools["costf"] = pools["coste"]
	router := costRouter(pools, usdRate("", "coste", 1), usdRate("", "costf", 1))
	s := getValueCost(t, router, "/api/v1/value")
	if !s.Available || len(s.Resources) != 1 || s.Compute == nil ||
		*s.Compute < 167 || *s.Compute > 169 {
		t.Fatalf("section = %+v, want one database priced once (~$168 for 7 days)", s)
	}
	if len(s.TopQueries) != 1 || s.TopQueries[0].QueryID != "5151" ||
		s.TopQueries[0].Database != "coste" || s.TopQueries[0].Share != 1 {
		t.Fatalf("top = %+v, want query 5151 of coste once with the whole share",
			s.TopQueries)
	}
}
