package autonomy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// clouds-07: provider storage capacity (cloudtel) is the denominator of
// custodian.wal.retained_wal_disk_pct_ceiling. Without it (no provider,
// unavailable or stale telemetry) the custodian proposes exactly what it
// proposed before; with it the custodian may only add proposals.

const gib = int64(1) << 30

// fakeDisk is a DiskCapacityProvider with a fixed answer.
type fakeDisk struct {
	bytes int64
	err   error
	calls atomic.Int64
}

func (d *fakeDisk) CapacityBytes(ctx context.Context) (int64, error) {
	d.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return d.bytes, d.err
}

// diskCustodian has a 25% disk ceiling and a 40 GiB byte backstop, so a
// slot retaining 30 GiB crosses only the disk ceiling of a 100 GiB disk.
func diskCustodian(t *testing.T, disk DiskCapacityProvider) *PostgresWALCustodian {
	t.Helper()
	return diskCustodianOn(requireAutonomyDB(t), disk)
}

// diskCustodianOn is diskCustodian on a pool the test already holds: each
// requireAutonomyDB opens a pool that stays connected until the test
// ends, so a test building many custodians shares one.
func diskCustodianOn(pool *pgxpool.Pool, disk DiskCapacityProvider) *PostgresWALCustodian {
	return NewPostgresWALCustodian(pool, "testdb", PostgresWALOptions{
		DiskPctCeiling: 25, RetainedBytesLimit: 40 * gib, Disk: disk,
	})
}

func activeSlot(name string, retained int64) walSlot {
	return walSlot{name: name, slotType: "logical", active: true, retained: retained}
}

func TestWALProviderCapacityProposesBoundWithEvidence(t *testing.T) {
	disk := &fakeDisk{bytes: 100 * gib}
	c := diskCustodian(t, disk)
	got, err := c.proposeFor(context.Background(), []walSlot{activeSlot("cdc", 30*gib)})
	if err != nil {
		t.Fatalf("proposeFor: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("proposals = %#v, want one bound", got)
	}
	p := got[0]
	if keep := proposedKeepBytes(t, p.SQL); keep < 45*gib {
		t.Fatalf("keep size %d below 1.5 x retained", keep)
	}
	if !reflect.DeepEqual(p.TargetObjects, []string{"slot:cdc"}) || p.Feature != "wal" {
		t.Fatalf("proposal = %#v", p)
	}
	want := map[string]any{"capacity_source": CapacitySourceProvider,
		"disk_capacity_bytes": 100 * gib, "disk_pct": 30.0, "retained_wal_bytes": 30 * gib}
	if !reflect.DeepEqual(p.Evidence, want) {
		t.Fatalf("evidence = %#v, want %#v", p.Evidence, want)
	}
	if disk.calls.Load() != 1 {
		t.Fatalf("capacity read %d times, want once per scan", disk.calls.Load())
	}
}

// Regression lock: no provider, an erroring (unavailable or stale)
// provider and a zero capacity all give today's proposals, byte for byte.
func TestWALWithoutProviderCapacityMatchesToday(t *testing.T) {
	slots := []walSlot{activeSlot("small", gib), activeSlot("disk_only", 30*gib),
		activeSlot("over_bytes", 41*gib),
		{name: "idle", slotType: "physical", active: false, retained: 50 * gib}}
	golden, err := diskCustodian(t, nil).proposeFor(context.Background(), slots)
	if err != nil {
		t.Fatalf("golden: %v", err)
	}
	if len(golden) != 1 || golden[0].Evidence != nil ||
		!reflect.DeepEqual(golden[0].TargetObjects, []string{"slot:over_bytes"}) {
		t.Fatalf("golden = %#v, want one evidence-free bound on over_bytes", golden)
	}
	cases := map[string]*fakeDisk{
		"unavailable": {err: errors.New("cloud telemetry unavailable: no credentials")},
		"stale":       {err: errors.New("cloud telemetry unavailable: no fresh storage sample")},
		"zero":        {bytes: 0},
		"negative":    {bytes: -1},
	}
	for name, disk := range cases {
		got, err := diskCustodian(t, disk).proposeFor(context.Background(), slots)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, golden) {
			t.Fatalf("%s proposals = %#v, want today's %#v", name, got, golden)
		}
	}
}

func TestWALUnboundedCapacityIsRecorded(t *testing.T) {
	disk := &fakeDisk{err: fmt.Errorf("cloud telemetry: %w", ErrDiskCapacityUnbounded)}
	slots := []walSlot{activeSlot("disk_only", 30*gib), activeSlot("over_bytes", 41*gib)}
	golden, err := diskCustodian(t, nil).proposeFor(context.Background(), slots)
	if err != nil {
		t.Fatal(err)
	}
	got, err := diskCustodian(t, disk).proposeFor(context.Background(), slots)
	if err != nil {
		t.Fatalf("proposeFor: %v", err)
	}
	if len(got) != 1 || got[0].SQL != golden[0].SQL {
		t.Fatalf("unbounded proposals = %#v, want today's bound %q", got, golden[0].SQL)
	}
	want := map[string]any{"capacity_source": CapacitySourceUnbounded,
		"retained_wal_bytes": 41 * gib}
	if !reflect.DeepEqual(got[0].Evidence, want) {
		t.Fatalf("unbounded evidence = %#v, want %#v", got[0].Evidence, want)
	}
}

// wal.Policy compares retained*100/capacity > ceiling: exactly 25% of the
// disk is not over the 25% ceiling, the next step above it is.
func TestWALDiskCeilingBoundary(t *testing.T) {
	capacity := 100 * gib
	cases := []struct {
		name     string
		retained int64
		want     int
	}{
		{"just below", capacity/4 - 1, 0},
		{"exactly 25%", capacity / 4, 0},
		{"25.01%", capacity * 2501 / 10000, 1},
	}
	for _, tc := range cases {
		c := diskCustodian(t, &fakeDisk{bytes: capacity})
		got, err := c.proposeFor(context.Background(), []walSlot{activeSlot("b", tc.retained)})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Fatalf("%s: %d proposals (%#v), want %d", tc.name, len(got), got, tc.want)
		}
		if tc.want == 1 && got[0].Evidence["disk_pct"] != 25.01 {
			t.Fatalf("%s: disk_pct = %v, want 25.01", tc.name, got[0].Evidence["disk_pct"])
		}
	}
}

func TestWALCapacityErrorDoesNotFailScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := diskCustodian(t, &fakeDisk{bytes: 100 * gib})
	cancel()
	if _, err := c.Scan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Scan err = %v, want context.Canceled", err)
	}
	if _, err := diskCustodian(t, &fakeDisk{err: errors.New("boom")}).Scan(
		context.Background()); err != nil {
		t.Fatalf("a failing capacity provider must not fail the scan: %v", err)
	}
}

// Concurrent scans share the inactive-slot clock; each still sees the
// provider capacity and proposes the same bound.
func TestWALConcurrentScansWithProviderCapacity(t *testing.T) {
	disk := &fakeDisk{bytes: 100 * gib}
	c := diskCustodian(t, disk)
	const workers = 8
	results := make([][]Proposal, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.proposeFor(context.Background(), []walSlot{
				activeSlot("cdc", 30*gib),
				{name: "idle", slotType: "logical", active: false, retained: 30 * gib}})
		}()
	}
	wg.Wait()
	for i := range workers {
		// The idle slot's bound duplicates the cluster-wide one: one per scan.
		if errs[i] != nil || len(results[i]) != 1 || results[i][0].SQL == "" ||
			results[i][0].Evidence["capacity_source"] != CapacitySourceProvider {
			t.Fatalf("worker %d = %#v, %v; want one provider-evidenced bound",
				i, results[i], errs[i])
		}
	}
	if disk.calls.Load() != workers {
		t.Fatalf("capacity reads = %d, want %d", disk.calls.Load(), workers)
	}
}

// Integration: a real logical slot retaining WAL on the live server. With
// provider capacity the custodian reads pg_replication_slots and proposes
// for the slot; without it the small retention proposes nothing.
func TestWALLiveLogicalSlotWithProviderCapacity(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	slot := fmt.Sprintf("clouds07_slot_%d", time.Now().UnixNano()%1_000_000)
	if _, err := pool.Exec(ctx, `SELECT pg_create_logical_replication_slot($1,
		'test_decoding')`, slot); err != nil {
		t.Fatalf("create logical slot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", slot)
	})
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS clouds07_wal (v text);
		INSERT INTO clouds07_wal SELECT repeat('x', 200) FROM generate_series(1, 50000);
		DROP TABLE clouds07_wal`); err != nil {
		t.Fatalf("generate WAL: %v", err)
	}
	var retained int64
	if err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(),
		restart_lsn)::bigint FROM pg_replication_slots WHERE slot_name=$1`,
		slot).Scan(&retained); err != nil || retained <= 0 {
		t.Fatalf("retained WAL = %d, %v", retained, err)
	}
	opts := PostgresWALOptions{DiskPctCeiling: 25}
	without, err := NewPostgresWALCustodian(pool, "testdb", opts).Scan(ctx)
	if err != nil {
		t.Fatalf("Scan without capacity: %v", err)
	}
	if p := proposalFor(without, slot); p != nil {
		t.Fatalf("without capacity proposal = %#v, want none", p)
	}
	opts.Disk = &fakeDisk{bytes: retained * 2}
	with, err := NewPostgresWALCustodian(pool, "testdb", opts).Scan(ctx)
	if err != nil {
		t.Fatalf("Scan with capacity: %v", err)
	}
	p := proposalFor(with, slot)
	if p == nil || p.Evidence["capacity_source"] != CapacitySourceProvider ||
		p.Evidence["disk_capacity_bytes"] != retained*2 {
		t.Fatalf("with capacity proposals = %#v, want one for %s with provider evidence",
			with, slot)
	}
	if pct, ok := p.Evidence["disk_pct"].(float64); !ok || pct < 40 || pct > 60 {
		t.Fatalf("disk_pct = %v, want about 50", p.Evidence["disk_pct"])
	}
}

func proposalFor(ps []Proposal, slot string) *Proposal {
	for i := range ps {
		if reflect.DeepEqual(ps[i].TargetObjects, []string{"slot:" + slot}) {
			return &ps[i]
		}
	}
	return nil
}
