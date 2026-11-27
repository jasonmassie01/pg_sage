//go:build integration

package gameday

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// benches-02 on real PostgreSQL: the fault programs act on the whole
// cluster, so a local game-day target is refused when it shares a cluster
// with a monitored (or control) database, whatever its DSN says.

func refusingPools(pools ...*pgxpool.Pool) RefusedFunc {
	return func(context.Context) ([]Querier, error) {
		out := make([]Querier, 0, len(pools))
		for _, p := range pools {
			out = append(out, p)
		}
		return out, nil
	}
}

func databaseOf(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Database
}

// markTarget sets (or clears, comment "") the game-day marker on dsn's
// database through admin.
func markTarget(t *testing.T, admin *pgxpool.Pool, dsn, comment string) {
	t.Helper()
	value := "NULL"
	if comment != "" {
		value = "'" + strings.ReplaceAll(comment, "'", "''") + "'"
	}
	sql := "COMMENT ON DATABASE " + pgx.Identifier{databaseOf(t, dsn)}.Sanitize() +
		" IS " + value
	if _, err := admin.Exec(context.Background(), sql); err != nil {
		t.Fatalf("mark %s: %v", databaseOf(t, dsn), err)
	}
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func createOn(t *testing.T, dsn string) error {
	t.Helper()
	p, err := NewLocalProvider(dsn, nil, refusingPools(testPool(t)))
	if err != nil {
		return err
	}
	_, err = p.Create(context.Background(), clone.CloneSpec{IncludeData: true})
	return err
}

type clusterState struct {
	maxWALSize string
	sbSlots    int
}

func readClusterState(t *testing.T, pool *pgxpool.Pool) clusterState {
	t.Helper()
	var s clusterState
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT setting FROM pg_catalog.pg_settings WHERE name = 'max_wal_size'),
		(SELECT count(*)::int FROM pg_catalog.pg_replication_slots
		  WHERE slot_name LIKE 'sb\_%')`).Scan(&s.maxWALSize, &s.sbSlots)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// CHECK-01 and CHECK-03: a marked sibling database on the monitored
// cluster is refused, and the game day leaves the cluster untouched.
func TestLocalTargetSiblingDatabaseIsRefused(t *testing.T) {
	f := newGDFixture(t)
	pool := testPool(t)
	sibling := testdb.CreateDatabase(t, "gd_sibling")
	markTarget(t, pool, sibling, TargetMarker)
	before := readClusterState(t, pool)
	p, err := NewLocalProvider(sibling, nil, refusingPools(pool))
	if err != nil {
		t.Fatalf("boot-time check refused a differently named database: %v", err)
	}
	r, err := NewRunner(Config{Database: f.database, Provider: "local",
		Now: func() time.Time { return f.now }}, p, f.faults, f.ledger, f.store)
	if err != nil {
		t.Fatal(err)
	}
	gd, err := r.Run(f.ctx)
	if !errors.Is(err, ErrMonitoredDSN) || gd.Status != StatusFailed {
		t.Fatalf("sibling game day = %+v (%v), want ErrMonitoredDSN", gd, err)
	}
	if len(f.faults.dsns) != 0 {
		t.Fatalf("fault programs ran on the monitored cluster: %d", len(f.faults.dsns))
	}
	if after := readClusterState(t, pool); after != before {
		t.Fatalf("cluster state changed: %+v -> %+v", before, after)
	}
}

// CHECK-02: another spelling of the monitored server's host.
func TestLocalTargetHostAliasIsRefused(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	alias := strings.Replace(dsn, "127.0.0.1", "localhost", 1)
	if alias == dsn {
		alias = strings.Replace(dsn, "localhost", "127.0.0.1", 1)
	}
	if alias == dsn {
		t.Fatalf("test DSN names neither localhost nor 127.0.0.1; cannot build an alias")
	}
	if err := createOn(t, alias); !errors.Is(err, ErrMonitoredDSN) {
		t.Fatalf("host alias: err = %v, want ErrMonitoredDSN", err)
	}
}

// The identity fallback works where pg_control_system is revoked (as on
// managed services), and refuses when the start time is revoked too
// (CHECK-06). Function privileges are per database, so the revokes stay
// in the sibling.
func TestLocalTargetIdentityFallbackOnRealPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sibling := testdb.CreateDatabase(t, "gd_probe")
	markTarget(t, pool, sibling, TargetMarker)
	role := fmt.Sprintf("gd_probe_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := pool.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'probe'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role) })
	cfg, err := pgx.ParseConfig(sibling)
	if err != nil {
		t.Fatal(err)
	}
	probeDSN := fmt.Sprintf("postgres://%s:probe@%s:%d/%s?sslmode=disable", role,
		cfg.Host, cfg.Port, cfg.Database)
	admin := openPool(t, sibling)
	if _, err := admin.Exec(ctx, "REVOKE EXECUTE ON FUNCTION "+
		"pg_catalog.pg_control_system() FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	probe, err := pgx.Connect(ctx, probeDSN)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ReadClusterIdentity(ctx, probe)
	_ = probe.Close(ctx)
	if err != nil || id.SystemID != "" || id.CatalogHash == "" || id.PostmasterStart.IsZero() {
		t.Fatalf("unprivileged identity = %+v (%v), want the catalog fallback", id, err)
	}
	if err := createOn(t, probeDSN); !errors.Is(err, ErrMonitoredDSN) {
		t.Fatalf("unprivileged sibling: err = %v, want ErrMonitoredDSN", err)
	}
	if _, err := admin.Exec(ctx, "REVOKE EXECUTE ON FUNCTION "+
		"pg_catalog.pg_postmaster_start_time() FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	if err := createOn(t, probeDSN); !errors.Is(err, ErrNotDisposable) {
		t.Fatalf("no privilege on either probe: err = %v, want ErrNotDisposable", err)
	}
}

// CHECK-05: a disposable second cluster with the marker completes a game
// day; without the marker it is refused.
func TestLocalTargetOnASecondCluster(t *testing.T) {
	f := newGDFixture(t)
	target := testdb.CreateDatabaseOnSecondCluster(t, "gd_target")
	admin := openPool(t, target)
	if err := createOn(t, target); !errors.Is(err, ErrNotDisposable) ||
		!strings.Contains(err.Error(), "COMMENT ON DATABASE") {
		t.Fatalf("unmarked second cluster: err = %v, want ErrNotDisposable naming the fix", err)
	}
	markTarget(t, admin, target, TargetMarker)
	p, err := NewLocalProvider(target, nil, refusingPools(testPool(t)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(Config{Database: f.database, Provider: "local",
		Now: func() time.Time { return f.now }}, p, f.faults, f.ledger, f.store)
	if err != nil {
		t.Fatal(err)
	}
	gd, err := r.Run(f.ctx)
	if err != nil || gd.Status != StatusCompleted || gd.CloneID != "local" {
		t.Fatalf("game day on the second cluster = %+v (%v)", gd, err)
	}
	if len(f.faults.dsns) != 1 || f.faults.dsns[0] != target {
		t.Fatalf("fault programs ran on %d targets", len(f.faults.dsns))
	}
}

// A streaming replica of the monitored primary is refused.
func TestLocalTargetReplicaOfTheMonitoredPrimaryIsRefused(t *testing.T) {
	replica := testdb.ReplicaDSN(t)
	err := createOn(t, replica)
	if !errors.Is(err, ErrMonitoredDSN) && !errors.Is(err, ErrNotDisposable) {
		t.Fatalf("replica: err = %v, want a refusal", err)
	}
	if !errors.Is(err, ErrMonitoredDSN) {
		t.Fatalf("replica shares the primary's system identifier: err = %v", err)
	}
}
