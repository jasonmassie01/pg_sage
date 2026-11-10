package main

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/runway"
)

// clouds-07: managed-cloud storage capacity reaches the WAL custodian
// (both construction sites) and the runway monitor. Without telemetry
// both keep today's inputs: a nil provider and the declared capacity.

const testGiB = int64(1) << 30

// capacitySource serves one sample, re-stamped at each collection.
type capacitySource struct {
	mu     sync.Mutex
	sample cloudtel.Sample
}

func (s *capacitySource) Provider() string { return "rds" }

func (s *capacitySource) Collect(_ context.Context, now time.Time) (cloudtel.Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sample
	out.CollectedAt = now
	return out, nil
}

type runwayClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *runwayClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// storageRuntime is a polled telemetry runtime whose sample reports
// capacityBytes of storage (auto-growing when grows).
func storageRuntime(t *testing.T, capacityBytes int64, grows bool) (*cloudtel.Runtime,
	*runwayClock) {
	t.Helper()
	clock := &runwayClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	at := clock.t.Add(-time.Minute)
	point := func(v int64) *cloudtel.Point { return &cloudtel.Point{Value: float64(v), At: at} }
	sample := cloudtel.Sample{Provider: "rds", Resource: "rds:us-east-1/orders",
		FreeStorageBytes: point(capacityBytes / 2), AllocatedStorageBytes: point(capacityBytes),
		StorageCapacityBytes: point(capacityBytes), StorageAutoGrows: grows}
	if grows {
		sample.StorageCapacityBytes = nil
	}
	r, err := cloudtel.NewRuntime(&capacitySource{sample: sample},
		cloudtel.RuntimeOptions{Database: "orders", Now: clock.now})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return r, clock
}

func TestDiskCapacityNilWithoutTelemetry(t *testing.T) {
	var typed *cloudtel.Runtime
	for name, rt := range map[string]*databaseRuntime{"no cloud": {},
		"typed nil": {cloud: typed}} {
		if disk := rt.diskCapacity(); disk != nil {
			t.Fatalf("%s: diskCapacity = %#v, want a nil interface", name, disk)
		}
		if f := rt.runwayCapacity(); f != nil {
			t.Fatalf("%s: runwayCapacity is set, want nil (declared capacity only)", name)
		}
	}
}

func TestDiskCapacityUnavailableTelemetryIsUnknown(t *testing.T) {
	rt := &databaseRuntime{cloud: cloudtel.Unavailable("orders", "rds", "no credentials")}
	disk := rt.diskCapacity()
	if disk == nil {
		t.Fatal("an unavailable runtime still wires a provider")
	}
	bytes, err := disk.CapacityBytes(context.Background())
	if err == nil || bytes != 0 || errors.Is(err, autonomy.ErrDiskCapacityUnbounded) {
		t.Fatalf("unavailable capacity = %d, %v; want unknown", bytes, err)
	}
	if b, src, ok := rt.runwayCapacity()(context.Background()); ok || b != 0 || src != "" {
		t.Fatalf("unavailable runway capacity = %v, %q, %v", b, src, ok)
	}
}

func TestDiskCapacityFreshProviderTelemetry(t *testing.T) {
	cloud, _ := storageRuntime(t, 100*testGiB, false)
	rt := &databaseRuntime{cloud: cloud}
	bytes, err := rt.diskCapacity().CapacityBytes(context.Background())
	if err != nil || bytes != 100*testGiB {
		t.Fatalf("CapacityBytes = %d, %v; want 100 GiB", bytes, err)
	}
	b, src, ok := rt.runwayCapacity()(context.Background())
	if !ok || b != float64(100*testGiB) || src != runway.CapacityProviderTelemetry {
		t.Fatalf("runway capacity = %v, %q, %v", b, src, ok)
	}
}

func TestDiskCapacityStaleTelemetryFallsBack(t *testing.T) {
	cloud, clock := storageRuntime(t, 100*testGiB, false)
	clock.mu.Lock()
	clock.t = clock.t.Add(cloudtel.MaxPointAge + time.Minute)
	clock.mu.Unlock()
	rt := &databaseRuntime{cloud: cloud}
	if b, err := rt.diskCapacity().CapacityBytes(context.Background()); err == nil || b != 0 {
		t.Fatalf("stale CapacityBytes = %d, %v; want unknown", b, err)
	}
	if b, src, ok := rt.runwayCapacity()(context.Background()); ok || src != "" || b != 0 {
		t.Fatalf("stale runway capacity = %v, %q, %v; want none (declared fallback)",
			b, src, ok)
	}
}

func TestDiskCapacityUnboundedStorage(t *testing.T) {
	cloud, _ := storageRuntime(t, 100*testGiB, true)
	rt := &databaseRuntime{cloud: cloud}
	_, err := rt.diskCapacity().CapacityBytes(context.Background())
	if !errors.Is(err, autonomy.ErrDiskCapacityUnbounded) {
		t.Fatalf("auto-growing storage err = %v, want ErrDiskCapacityUnbounded", err)
	}
	if b, src, ok := rt.runwayCapacity()(context.Background()); ok || b != 0 ||
		src != runway.CapacityUnbounded {
		t.Fatalf("unbounded runway capacity = %v, %q, %v", b, src, ok)
	}
}

func TestRunwayMonitorOptionsCarryProviderCapacity(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Forecaster.DiskCapacityBytes = 50 * testGiB
	plain := &databaseRuntime{cfg: cfg, spec: databaseRuntimeSpec{Name: "orders"}}
	got := plain.runwayMonitorOptions()
	if got.Capacity != nil || !reflect.DeepEqual(got, runwayOptions(cfg, "orders")) {
		t.Fatalf("without telemetry options = %+v, want today's runwayOptions", got)
	}
	cloud, _ := storageRuntime(t, 100*testGiB, false)
	managed := &databaseRuntime{cfg: cfg, spec: databaseRuntimeSpec{Name: "orders"},
		cloud: cloud}
	o := managed.runwayMonitorOptions()
	if o.Capacity == nil || o.DiskCapacityBytes != float64(50*testGiB) {
		t.Fatalf("managed options = %+v, want provider capacity and the declared fallback", o)
	}
	if b, src, ok := o.Capacity(context.Background()); !ok || b != float64(100*testGiB) ||
		src != runway.CapacityProviderTelemetry {
		t.Fatalf("managed capacity = %v, %q, %v", b, src, ok)
	}
}

// Both WAL custodian construction sites take the provider: the custodian
// built for the autonomy loop and for the runway advisor proposes for a
// slot only provider capacity puts over the disk ceiling.
func TestWALCustodianSitesUseProviderCapacity(t *testing.T) {
	pool := autonomyPool(t)
	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.Custodian.WAL.RetainedWALDiskPctCeiling = 25
	slot := "clouds07_wiring_slot"
	retained := retainingSlot(t, pool, slot)
	cloud, _ := storageRuntime(t, retained*2, false)
	rt := &databaseRuntime{cloud: cloud}
	for name, disk := range map[string]autonomy.DiskCapacityProvider{
		"telemetry": rt.diskCapacity(), "none": (&databaseRuntime{}).diskCapacity()} {
		got, err := newWALCustodian(pool, cfg, "orders", disk).Scan(ctx)
		if err != nil {
			t.Fatalf("%s: Scan: %v", name, err)
		}
		found := slotEvidence(got, slot)
		if name == "none" && found != nil {
			t.Fatalf("without telemetry the slot was proposed for: %#v", got)
		}
		if name == "telemetry" && (found == nil ||
			found["capacity_source"] != autonomy.CapacitySourceProvider) {
			t.Fatalf("with telemetry proposals = %#v, want provider evidence", got)
		}
	}
	advisor := newRunwayAdvisorFor(newFreezeCustodian(pool, cfg, "orders"), pool, cfg,
		"orders", rt.diskCapacity(), nil)
	got, err := advisor.wal.Scan(ctx)
	if err != nil {
		t.Fatalf("advisor WAL Scan: %v", err)
	}
	if ev := slotEvidence(got, slot); ev == nil ||
		ev["capacity_source"] != autonomy.CapacitySourceProvider {
		t.Fatalf("runway advisor's WAL custodian proposals = %#v, want provider evidence", got)
	}
}

func slotEvidence(ps []autonomy.Proposal, slot string) map[string]any {
	for _, p := range ps {
		if reflect.DeepEqual(p.TargetObjects, []string{"slot:" + slot}) {
			return p.Evidence
		}
	}
	return nil
}

// retainingSlot creates a logical slot (dropped at cleanup) and writes WAL
// past it, returning the WAL it retains.
func retainingSlot(t *testing.T, pool *pgxpool.Pool, slot string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT pg_create_logical_replication_slot($1,
		'test_decoding')`, slot); err != nil {
		t.Fatalf("create logical slot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", slot)
	})
	if _, err := pool.Exec(ctx, `CREATE TABLE clouds07_wiring (v text);
		INSERT INTO clouds07_wiring SELECT repeat('x', 200) FROM generate_series(1, 50000);
		DROP TABLE clouds07_wiring`); err != nil {
		t.Fatalf("generate WAL: %v", err)
	}
	var retained int64
	if err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(),
		restart_lsn)::bigint FROM pg_replication_slots WHERE slot_name=$1`,
		slot).Scan(&retained); err != nil || retained <= 0 {
		t.Fatalf("retained = %d, %v", retained, err)
	}
	return retained
}
