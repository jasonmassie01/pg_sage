package cost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// fakeReader serves per-database deltas and per-resource latest hours.
type fakeReader struct {
	mu     sync.Mutex
	deltas map[string][]QueryDelta
	hours  map[string]*HourRow
	errs   map[string]error
	reads  map[string]int
}

func (f *fakeReader) Deltas(_ context.Context, m Member, _, _ time.Time) ([]QueryDelta,
	int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	f.reads[m.Database]++
	if err := f.errs[m.Database]; err != nil {
		return nil, 0, err
	}
	return f.deltas[m.Database], 0, nil
}

func (f *fakeReader) LatestHour(_ context.Context, m Member) (*HourRow, error) {
	return f.hours[m.Resource], nil
}

var (
	repTo   = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	repFrom = repTo.Add(-10 * time.Hour)
)

func ratesFor(entries ...config.CostRate) RateSource {
	return NewConfigRates(config.CostConfig{Rates: entries})
}

func rateEntry(resource, database string, perHour float64) config.CostRate {
	return config.CostRate{Resource: resource, Database: database, Currency: "USD",
		AsOf: "2026-11-01", ComputePerHour: &perHour, StorageGBMonth: 0.1}
}

func TestBuild_NoRatesIsUnavailableWithAReason(t *testing.T) {
	s := Build(context.Background(), &fakeReader{}, []Member{{Database: "orders"}}, nil,
		repFrom, repTo)
	if s.Available || s.Reason != ReasonNotConfigured || s.Total != nil ||
		s.RateSource != "" || len(s.TopQueries) != 0 {
		t.Fatalf("section = %+v, want unavailable: rates not configured", s)
	}
	empty := Build(context.Background(), &fakeReader{}, []Member{{Database: "orders"}},
		ratesFor(), repFrom, repTo)
	if empty.Available || empty.Reason != ReasonNotConfigured {
		t.Fatalf("empty rates: %+v, want rates not configured", empty)
	}
}

func TestBuild_NoMembers(t *testing.T) {
	s := Build(context.Background(), &fakeReader{}, nil,
		ratesFor(rateEntry("", "orders", 1)), repFrom, repTo)
	if s.Available || s.Reason == "" {
		t.Fatalf("section = %+v, want unavailable with a reason", s)
	}
}

func TestBuild_EmptyQueryStoreIsAnEmptyList(t *testing.T) {
	s := Build(context.Background(), &fakeReader{}, []Member{{Database: "orders"}},
		ratesFor(rateEntry("", "orders", 0.5)), repFrom, repTo)
	if !s.Available || s.TopQueries == nil || len(s.TopQueries) != 0 {
		t.Fatalf("section = %+v, want available with an empty top list", s)
	}
	if s.Compute == nil || !near(*s.Compute, 5) || s.RateSource != SourceConfig ||
		s.AsOf != "2026-11-01" || s.Currency != "USD" || !near(s.WindowHours, 10) {
		t.Fatalf("section = %+v, want $5 compute over 10 h from config as of 2026-11-01", s)
	}
}

func TestBuild_FleetCountsTheInstanceOnce(t *testing.T) {
	r := &fakeReader{
		deltas: map[string][]QueryDelta{
			"orders":  {{Database: "orders", QueryID: 1, ExecMs: 3_000_000}},
			"billing": {{Database: "billing", QueryID: 2, ExecMs: 1_000_000}},
		},
		hours: map[string]*HourRow{"rds:us-east-1/shared": {VCPUs: 2,
			AllocatedStorageBytes: 100 * gb}},
	}
	members := []Member{{Database: "orders", Resource: "rds:us-east-1/shared"},
		{Database: "billing", Resource: "rds:us-east-1/shared"}}
	s := Build(context.Background(), r, members,
		ratesFor(rateEntry("rds:us-east-1/shared", "", 2)), repFrom, repTo)
	if !s.Available || s.Compute == nil || !near(*s.Compute, 20) {
		t.Fatalf("compute = %v, want $20 (2/h x 10 h, once for the shared instance)",
			s.Compute)
	}
	if len(s.Resources) != 1 || len(s.Resources[0].Databases) != 2 {
		t.Fatalf("resources = %+v, want one resource naming both databases", s.Resources)
	}
	if len(s.TopQueries) != 2 || s.TopQueries[0].Database != "orders" ||
		!near(s.TopQueries[0].Cost, 15) || !near(s.TopQueries[1].Cost, 5) {
		t.Fatalf("top = %+v, want 15/5 split across the databases", s.TopQueries)
	}
	// 4000 s over 10 h x 2 vCPUs = 72000 vCPU-s.
	if s.IdleShare == nil || !near(*s.IdleShare, 1-4000.0/72000) {
		t.Fatalf("idle = %v", s.IdleShare)
	}
	// 100 GiB x $0.1/GB-month x 10 h / 730 h.
	if s.Storage == nil || !near(*s.Storage, 100*0.1*10/730) {
		t.Fatalf("storage = %v", s.Storage)
	}
	if s.Total == nil || !near(*s.Total, 20+100*0.1*10/730) {
		t.Fatalf("total = %v", s.Total)
	}
}

func TestBuild_UnpricedDatabaseIsNamedNotGuessed(t *testing.T) {
	members := []Member{{Database: "orders"}, {Database: "scratch"}}
	s := Build(context.Background(), &fakeReader{}, members,
		ratesFor(rateEntry("", "orders", 1)), repFrom, repTo)
	if !s.Available || len(s.Unpriced) != 1 || s.Unpriced[0] != "scratch" ||
		!near(*s.Compute, 10) {
		t.Fatalf("section = %+v, want scratch unpriced and only orders costed", s)
	}
}

func TestBuild_ReaderErrorMakesItPartial(t *testing.T) {
	r := &fakeReader{errs: map[string]error{"orders": errors.New("connection refused")},
		deltas: map[string][]QueryDelta{"billing": {{Database: "billing", QueryID: 5,
			ExecMs: 10}}}}
	members := []Member{{Database: "orders"}, {Database: "billing"}}
	s := Build(context.Background(), r, members,
		ratesFor(rateEntry("", "orders", 1), rateEntry("", "billing", 1)), repFrom, repTo)
	if !s.Partial || len(s.Unavailable) != 1 ||
		!strings.Contains(s.Unavailable[0], "orders") ||
		!strings.Contains(s.Unavailable[0], "connection refused") {
		t.Fatalf("section = %+v, want partial naming orders and the error", s)
	}
	if len(s.TopQueries) != 1 || s.TopQueries[0].Database != "billing" {
		t.Fatalf("top = %+v, want billing still attributed", s.TopQueries)
	}
}

func TestBuild_TopListIsCappedAtTwenty(t *testing.T) {
	var deltas []QueryDelta
	for i := 1; i <= 30; i++ {
		deltas = append(deltas, QueryDelta{Database: "orders", QueryID: int64(i),
			ExecMs: float64(i)})
	}
	r := &fakeReader{deltas: map[string][]QueryDelta{"orders": deltas}}
	s := Build(context.Background(), r, []Member{{Database: "orders"}},
		ratesFor(rateEntry("", "orders", 1)), repFrom, repTo)
	if len(s.TopQueries) != TopQueries || s.TopQueries[0].QueryID != "30" ||
		s.TopQueries[19].QueryID != "11" {
		t.Fatalf("top = %d entries, first %v", len(s.TopQueries), s.TopQueries[0])
	}
}

func TestBuild_MixedCurrenciesHaveNoTotal(t *testing.T) {
	eur := rateEntry("", "billing", 1)
	eur.Currency = "EUR"
	members := []Member{{Database: "orders"}, {Database: "billing"}}
	s := Build(context.Background(), &fakeReader{}, members,
		ratesFor(rateEntry("", "orders", 1), eur), repFrom, repTo)
	if !s.Available || s.Total != nil || s.Compute != nil || s.Currency != "mixed" ||
		len(s.Resources) != 2 {
		t.Fatalf("section = %+v, want per-resource figures only", s)
	}
}

func TestBuild_DeclaredVCPUsForSelfManaged(t *testing.T) {
	e := rateEntry("", "orders", 1)
	e.VCPUs = 4
	r := &fakeReader{deltas: map[string][]QueryDelta{"orders": {{Database: "orders",
		QueryID: 1, ExecMs: 36_000_000}}}}
	s := Build(context.Background(), r, []Member{{Database: "orders"}}, ratesFor(e),
		repFrom, repTo)
	if s.IdleShare == nil || !near(*s.IdleShare, 0.75) {
		t.Fatalf("idle = %v, want 0.75 from the declared 4 vCPUs", s.IdleShare)
	}
}

func TestBuild_ConcurrentReadsAreSafe(t *testing.T) {
	r := &fakeReader{deltas: map[string][]QueryDelta{}}
	var members []Member
	var entries []config.CostRate
	for i := range 12 {
		name := fmt.Sprintf("db%02d", i)
		members = append(members, Member{Database: name})
		entries = append(entries, rateEntry("", name, 1))
		r.deltas[name] = []QueryDelta{{Database: name, QueryID: int64(i + 1), ExecMs: 100}}
	}
	s := Build(context.Background(), r, members, ratesFor(entries...), repFrom, repTo)
	if len(s.TopQueries) != 12 || s.Compute == nil || !near(*s.Compute, 120) {
		t.Fatalf("section = %+v", s)
	}
	for name, n := range r.reads {
		if n != 1 {
			t.Fatalf("%s read %d times, want once", name, n)
		}
	}
}

func TestConfigRates_Lookup(t *testing.T) {
	rs := ratesFor(rateEntry("rds:us-east-1/a", "", 2), rateEntry("", "selfhosted", 3))
	if r, ok := rs.Rates("rds:us-east-1/a", "orders"); !ok || r.ComputePerHour != 2 ||
		r.Source != SourceConfig || r.AsOf.Format("2006-01-02") != "2026-11-01" {
		t.Fatalf("by resource: %+v %v", r, ok)
	}
	if r, ok := rs.Rates("", "selfhosted"); !ok || r.ComputePerHour != 3 {
		t.Fatalf("by database: %+v %v", r, ok)
	}
	if _, ok := rs.Rates("rds:us-east-1/b", "other"); ok {
		t.Fatal("an unconfigured target must have no rates, never another's")
	}
	var nilRates *ConfigRates
	if _, ok := nilRates.Rates("x", "y"); ok {
		t.Fatal("nil rates must report none")
	}
}

// Fleet: two entries for one physical database (same Identity) are read
// and priced once, as the value ledger counts them; the later name's rate
// still prices it.
func TestBuild_SamePhysicalDatabaseCountedOnce(t *testing.T) {
	r := &fakeReader{deltas: map[string][]QueryDelta{
		"app":  {{Database: "app", QueryID: 7, ExecMs: 1000}},
		"app2": {{Database: "app2", QueryID: 7, ExecMs: 1000}},
	}}
	members := []Member{{Database: "app", Identity: "sys/app"},
		{Database: "app2", Identity: "sys/app"}, {Database: "other", Identity: "sys/other"}}
	s := Build(context.Background(), r, members,
		ratesFor(rateEntry("", "app2", 1), rateEntry("", "other", 1)), repFrom, repTo)
	if !s.Available || len(s.Resources) != 2 || len(s.Unpriced) != 0 {
		t.Fatalf("section = %+v, want two priced databases (app once, other)", s)
	}
	if s.Compute == nil || !near(*s.Compute, 20) {
		t.Fatalf("compute = %v, want $20 ($1/h x 10 h for app once and for other)",
			s.Compute)
	}
	if len(s.TopQueries) != 1 || s.TopQueries[0].Database != "app" ||
		!near(s.TopQueries[0].Share, 1) || !near(s.TopQueries[0].Cost, 10) {
		t.Fatalf("top = %+v, want query 7 of app once at $10", s.TopQueries)
	}
	if r.reads["app2"] != 0 || r.reads["app"] != 1 {
		t.Fatalf("reads = %v, want the duplicate entry never read", r.reads)
	}
}
