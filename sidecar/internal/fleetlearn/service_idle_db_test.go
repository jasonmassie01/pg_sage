package fleetlearn

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// sagePool opens a pool on plain's database configured as the sidecar's
// per-database pools are (selfmonitor.ConfigurePool), so the service's
// statements carry the pg_sage tag where pg_stat_statements keeps it.
func sagePool(t *testing.T, plain *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(plain.Config().ConnString())
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	selfmonitor.ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect pg_sage pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// pgssReset clears pool's database's pg_stat_statements entries, so a
// later count sees only statements run after it.
func pgssReset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	exec(t, pool, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	exec(t, pool, `SELECT pg_stat_statements_reset(0, (SELECT oid FROM
		pg_catalog.pg_database WHERE datname = current_database()), 0)`)
}

// pgSageStatements counts pg_sage's statements recorded on pool's database
// since the last reset (the fingerprint and digest reads), by the rule
// pg_sage's readers exclude them with: a statement whose text has no
// pg_sage tag and no sage schema reference is not pg_sage's. A prefix
// match would miss them: PostgreSQL 18 drops a leading comment from the
// stored text, and pg_sage's pools move the tag after the first keyword.
func pgSageStatements(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_catalog.pg_database
		WHERE datname = current_database()) AND NOT (`+
		selfmonitor.StatementExclusionSQL("query")+`)`).Scan(&n)
	if err != nil {
		t.Fatalf("count pg_stat_statements: %v", err)
	}
	return n
}

func fingerprintRows(t *testing.T, control *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := control.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.fleet_fingerprint`).Scan(&n); err != nil {
		t.Fatalf("count fingerprints: %v", err)
	}
	return n
}

func TestService_StandaloneCycleReadsAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	control := freshDB(t, "idle_control")
	alone := freshDB(t, "idle_alone")
	tenantSchema(t, alone, "orders", "customers")
	pgssReset(t, alone)
	srcs := []DatabaseSource{{Name: "alone", Pool: sagePool(t, alone)}}
	svc := NewService(NewStore(control, "standalone:alone"),
		func() []DatabaseSource { return srcs }, Settings{}, nil)
	for i := 0; i < 3; i++ {
		res, err := svc.RunCycle(ctx, Fence{})
		if err != nil || res.Skipped != SkipNoPeer {
			t.Fatalf("standalone cycle %d = %+v, %v; want skipped", i, res, err)
		}
	}
	if n := fingerprintRows(t, control); n != 0 {
		t.Fatalf("a standalone sidecar wrote %d fingerprints", n)
	}
	if n := pgSageStatements(t, alone); n != 0 {
		t.Fatalf("a standalone cycle ran %d fingerprint queries on the database", n)
	}
	// Positive control: once a peer shares the boundary, the same probe sees
	// the fingerprint reads and both databases are fingerprinted.
	peer := freshDB(t, "idle_peer")
	tenantSchema(t, peer, "purchases", "clients")
	srcs = append(srcs, DatabaseSource{Name: "peer", Pool: sagePool(t, peer)})
	res, err := svc.RunCycle(ctx, Fence{})
	if err != nil || res.Skipped != "" || res.Databases != 2 {
		t.Fatalf("eligible cycle = %+v, %v; want 2 databases", res, err)
	}
	if n := fingerprintRows(t, control); n != 2 {
		t.Fatalf("eligible cycle wrote %d fingerprints, want 2", n)
	}
	if n := pgSageStatements(t, alone); n == 0 {
		t.Fatal("the probe saw no fingerprint query on an eligible cycle")
	}
}

func TestService_TenantsWithoutPeersWriteNothing(t *testing.T) {
	ctx := context.Background()
	control := freshDB(t, "idle_tctl")
	one, two := freshDB(t, "idle_t1"), freshDB(t, "idle_t2")
	svc := NewService(NewStore(control, "meta"), func() []DatabaseSource {
		return []DatabaseSource{{Name: "one", Boundary: "tenant:one", Pool: one},
			{Name: "two", Boundary: "tenant:two", Pool: two}}
	}, Settings{}, nil)
	res, err := svc.RunCycle(ctx, Fence{})
	if err != nil || res.Skipped != SkipNoPeer {
		t.Fatalf("two tenants = %+v, %v; want skipped", res, err)
	}
	if n := fingerprintRows(t, control); n != 0 {
		t.Fatalf("tenants without peers wrote %d fingerprints", n)
	}
}

func TestService_ShrunkFleetKeepsFingerprintsUntilEligible(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	if _, err := f.svc.RunCycle(ctx, Fence{}); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	store := NewStore(f.control, "test-scope")
	lone := NewService(store, func() []DatabaseSource { return f.sources[:1] },
		Settings{}, nil)
	res, err := lone.RunCycle(ctx, Fence{})
	if err != nil || res.Skipped != SkipNoPeer {
		t.Fatalf("shrunk fleet = %+v, %v; want skipped", res, err)
	}
	if n := fingerprintRows(t, f.control); n != 5 {
		t.Fatalf("a skipped cycle pruned the fleet: %d fingerprints left", n)
	}
	looks, err := lone.LookAlikes(ctx, "c")
	if err != nil || len(looks) != 2 {
		t.Fatalf("leftover fingerprints unreadable: %+v, %v", looks, err)
	}
	pair := NewService(store, func() []DatabaseSource { return f.sources[:2] },
		Settings{}, nil)
	if res, err := pair.RunCycle(ctx, Fence{}); err != nil || res.Databases != 2 {
		t.Fatalf("eligible again = %+v, %v", res, err)
	}
	if n := fingerprintRows(t, f.control); n != 2 {
		t.Fatalf("the next eligible cycle kept %d fingerprints, want 2", n)
	}
}
