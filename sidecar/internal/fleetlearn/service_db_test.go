package fleetlearn

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type fleetFixture struct {
	control *pgxpool.Pool
	sources []DatabaseSource
	svc     *Service
}

// newFleet builds three tenant databases with the same shape (a, b, c), one
// with a different shape (odd) and one same-shaped database in another
// tenant (foreign). a, b and foreign carry verified outcomes.
func newFleet(t *testing.T) fleetFixture {
	t.Helper()
	control := freshDB(t, "svc_control")
	mk := func(label, boundary, orders, customers string) DatabaseSource {
		pool := freshDB(t, "svc_"+label)
		tenantSchema(t, pool, orders, customers)
		return DatabaseSource{Name: label, Boundary: boundary, Pool: pool}
	}
	a := mk("a", "", "orders", "customers")
	b := mk("b", "", "orders", "customers")
	c := mk("c", "", "purchases", "clients")
	foreign := mk("foreign", "tenant:other", "orders", "customers")
	odd := freshDB(t, "svc_odd")
	exec(t, odd, "CREATE TABLE metrics (ts timestamptz, v double precision)")
	for i := 0; i < 3; i++ {
		seedOutcome(t, a.Pool, "index_create", "public.orders", "improved")
		seedOutcome(t, b.Pool, "index_create", "public.orders", "improved")
	}
	seedOutcome(t, b.Pool, "index_create", "public.orders", "regressed")
	for i := 0; i < 10; i++ {
		seedOutcome(t, foreign.Pool, "index_create", "public.orders", "regressed")
	}
	sources := []DatabaseSource{a, b, c, foreign,
		{Name: "odd", Boundary: "", Pool: odd}}
	svc := NewService(NewStore(control, "test-scope"), func() []DatabaseSource {
		return sources
	}, Settings{MinSimilarity: 0.6, MinPriorOutcomes: 3, WindowDays: 30}, nil)
	return fleetFixture{control: control, sources: sources, svc: svc}
}

func TestService_CycleThenPriorFromLookAlikesOnly(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	res, err := f.svc.RunCycle(ctx, Fence{})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if res.Databases != 5 || len(res.Failed) != 0 {
		t.Fatalf("cycle result = %+v, want 5 databases, none failed", res)
	}
	looks, err := f.svc.LookAlikes(ctx, "c")
	if err != nil {
		t.Fatalf("look-alikes: %v", err)
	}
	names := []string{}
	for _, l := range looks {
		names = append(names, l.Database)
	}
	if strings.Join(names, ",") != "a,b" && strings.Join(names, ",") != "b,a" {
		t.Fatalf("c's look-alikes = %v, want a and b (not foreign, not odd)", names)
	}
	p, ok, err := f.svc.Prior(ctx, "c", "index_create", []string{"public.purchases"})
	if err != nil || !ok {
		t.Fatalf("prior = %+v ok=%v err=%v", p, ok, err)
	}
	if p.Match != MatchTableShape || p.Improved != 6 || p.Regressed != 1 ||
		p.Databases != 2 {
		t.Fatalf("prior = %+v, want 6 improved 1 regressed on 2 look-alikes", p)
	}
	if p.Cautions() {
		t.Fatal("a mostly-improved prior must not caution")
	}
}

func TestService_PriorNeverCrossesTenants(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	if _, err := f.svc.RunCycle(ctx, Fence{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	p, ok, err := f.svc.Prior(ctx, "a", "index_create", []string{"orders"})
	if err != nil || !ok {
		t.Fatalf("a's prior = %+v ok=%v err=%v", p, ok, err)
	}
	if p.Regressed != 1 {
		t.Fatalf("a's prior saw %d regressions; the other tenant's 10 leaked in", p.Regressed)
	}
	if _, ok, _ := f.svc.Prior(ctx, "foreign", "index_create",
		[]string{"orders"}); ok {
		t.Fatal("the foreign tenant got a prior from the operator's databases")
	}
}

func TestService_UnknownAndUnfingerprinted(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	looks, err := f.svc.LookAlikes(ctx, "a")
	if err != nil || len(looks) != 0 {
		t.Fatalf("before any cycle = %+v,%v want none", looks, err)
	}
	if _, ok, err := f.svc.Prior(ctx, "nobody", "index_create", nil); ok || err != nil {
		t.Fatalf("unknown database prior ok=%v err=%v", ok, err)
	}
	if _, err := f.svc.LookAlikes(ctx, ""); !errors.Is(err, ErrDatabaseRequired) {
		t.Fatalf("empty database err = %v, want ErrDatabaseRequired", err)
	}
}

func TestService_CycleReportsAFailedDatabaseAndKeepsGoing(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	dead, err := pgxpool.New(ctx, "postgres://x@127.0.0.1:1/x?connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	dead.Close()
	sources := append([]DatabaseSource{}, f.sources...)
	sources = append(sources, DatabaseSource{Name: "dead", Pool: dead})
	svc := NewService(NewStore(f.control, "test-scope"),
		func() []DatabaseSource { return sources }, Settings{}, nil)
	res, err := svc.RunCycle(ctx, Fence{})
	if err != nil {
		t.Fatalf("one bad database must not fail the cycle: %v", err)
	}
	if res.Databases != 5 || len(res.Failed) != 1 || res.Failed[0] != "dead" {
		t.Fatalf("result = %+v, want 5 ok and dead failed", res)
	}
}

func TestService_FencedCycleStopsOnLostLease(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	_, err := f.svc.RunCycle(ctx, Fence{Holder: "me", Epoch: 1})
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("cycle without the lease err = %v, want ErrFenced", err)
	}
	got, _ := NewStore(f.control, "test-scope").Fingerprints(ctx)
	if len(got) != 0 {
		t.Fatalf("a fenced cycle wrote %d fingerprints", len(got))
	}
}

func TestService_PruneDepartedOnCycle(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	if _, err := f.svc.RunCycle(ctx, Fence{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	keep := f.sources[:2]
	svc := NewService(NewStore(f.control, "test-scope"),
		func() []DatabaseSource { return keep }, Settings{}, nil)
	if _, err := svc.RunCycle(ctx, Fence{}); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	got, _ := NewStore(f.control, "test-scope").Fingerprints(ctx)
	if len(got) != 2 {
		t.Fatalf("departed databases kept: %d fingerprints", len(got))
	}
}

func TestCollectFleetFindings_FansOutAndGroups(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	for _, s := range f.sources[:3] {
		exec(t, s.Pool, `INSERT INTO sage.findings (category, severity, object_type,
			object_identifier, title, detail, status, recommendation)
			VALUES ('missing_index','warning','table','public.orders|btree(status)',
			'Missing index on public.orders','{}','open','CREATE INDEX ...')`)
	}
	exec(t, f.sources[3].Pool, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status)
		VALUES ('missing_index','warning','table','public.orders|btree(status)',
		't','{}','resolved')`)
	srcs := make([]Source, 0, len(f.sources))
	for _, s := range f.sources {
		srcs = append(srcs, Source{Name: s.Name, Pool: s.Pool})
	}
	res := CollectFleetFindings(ctx, srcs, 3, 100)
	if res.DatabasesScanned != 5 || len(res.Errors) != 0 {
		t.Fatalf("scanned=%d errors=%v", res.DatabasesScanned, res.Errors)
	}
	if len(res.Findings) != 1 || res.Findings[0].Databases != 3 {
		t.Fatalf("fleet findings = %+v, want one on 3 databases (resolved ignored)",
			res.Findings)
	}
	occ := res.Findings[0].Occurrences
	if occ[0].ID == 0 || occ[0].Database != "a" || occ[0].Recommendation == "" {
		t.Fatalf("drill-down row = %+v", occ[0])
	}
	if got := CollectFleetFindings(ctx, srcs, 4, 100); len(got.Findings) != 0 {
		t.Fatalf("min 4 = %+v, want none", got.Findings)
	}
}

func TestCollectFleetFindings_PartialFailureAndEmpty(t *testing.T) {
	ctx := context.Background()
	good := freshDB(t, "coll_good")
	dead, err := pgxpool.New(ctx, "postgres://x@127.0.0.1:1/x?connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	dead.Close()
	res := CollectFleetFindings(ctx, []Source{{Name: "good", Pool: good},
		{Name: "dead", Pool: dead}, {Name: "nil"}}, 2, 10)
	if res.DatabasesScanned != 1 || len(res.Errors) != 2 {
		t.Fatalf("scanned=%d errors=%+v, want 1 scanned and 2 errors",
			res.DatabasesScanned, res.Errors)
	}
	if res.Errors[0].Database == "" || res.Errors[0].Error == "" {
		t.Fatalf("errors must name the database and the cause: %+v", res.Errors)
	}
	if res.Findings == nil {
		t.Fatal("findings must be an empty list, not nil")
	}
	empty := CollectFleetFindings(ctx, nil, 2, 10)
	if empty.DatabasesScanned != 0 || empty.Findings == nil {
		t.Fatalf("no sources = %+v", empty)
	}
}

func TestService_LookAlikesCacheRefreshesAfterTTL(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	now := time.Now()
	f.svc.now = func() time.Time { return now }
	looks, _ := f.svc.LookAlikes(ctx, "c")
	if len(looks) != 0 {
		t.Fatalf("before a cycle: %+v", looks)
	}
	if _, err := f.svc.RunCycle(ctx, Fence{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	// RunCycle invalidates the cache, so the new rows are visible at once.
	looks, _ = f.svc.LookAlikes(ctx, "c")
	if len(looks) != 2 {
		t.Fatalf("after a cycle: %+v, want 2", looks)
	}
	// Another sidecar's cycle is seen after the cache TTL.
	exec(t, f.control, `DELETE FROM sage.fleet_fingerprint WHERE database_name='a'`)
	looks, _ = f.svc.LookAlikes(ctx, "c")
	if len(looks) != 2 {
		t.Fatalf("within the TTL the cache must serve: %+v", looks)
	}
	now = now.Add(DefaultCacheTTL + time.Second)
	looks, _ = f.svc.LookAlikes(ctx, "c")
	if len(looks) != 1 {
		t.Fatalf("after the TTL: %+v, want only b", looks)
	}
}

func TestService_CycleWithNothingReadKeepsStoredFingerprints(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	if _, err := f.svc.RunCycle(ctx, Fence{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	none := NewService(NewStore(f.control, "test-scope"),
		func() []DatabaseSource { return nil }, Settings{}, nil)
	res, err := none.RunCycle(ctx, Fence{})
	if err != nil || res.Databases != 0 {
		t.Fatalf("empty cycle = %+v, %v", res, err)
	}
	dead := NewService(NewStore(f.control, "test-scope"),
		func() []DatabaseSource { return []DatabaseSource{{Name: "x"}} }, Settings{}, nil)
	if res, err := dead.RunCycle(ctx, Fence{}); err != nil || len(res.Failed) != 1 {
		t.Fatalf("all-failed cycle = %+v, %v", res, err)
	}
	got, _ := NewStore(f.control, "test-scope").Fingerprints(ctx)
	if len(got) != 5 {
		t.Fatalf("a cycle that read nothing pruned the fleet: %d left", len(got))
	}
}
