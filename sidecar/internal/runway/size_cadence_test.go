package runway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Size sampling cadence (v2.3.x): the databases' total size
// (cluster_database_size stats every file of every database, 105-250 ms
// at 20,000 relations) is measured at most every SizeInterval and reused
// in between; it is sampled only when newly measured, so a trend never
// sees one measurement twice. WAL and the other series still sample
// every tick.

// sizeCadenceRunner answers wal_runway for a primary and counts the size
// measurements; failSize fails them.
type sizeCadenceRunner struct {
	mu       sync.Mutex
	sizes    int
	bytes    int64
	failSize bool
}

func (r *sizeCadenceRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch id {
	case probes.WALRunwayProbe:
		return probes.Result{ProbeID: id, Status: probes.StatusOK, Rows: []probes.Row{{
			"wal_position_bytes": int64(9e9), "in_recovery": false,
			"system_identifier": "7001", "role_name": "sage",
			"server_started_at": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}}}
	case probes.ClusterDatabaseSizeProbe:
		r.sizes++
		if r.failSize {
			return sizeFailed()
		}
		r.bytes += 1 << 20
		return sizeOK(r.bytes)
	}
	return probes.Result{ProbeID: id, Status: probes.StatusEmpty}
}

func (r *sizeCadenceRunner) RunBackground(ctx context.Context, id probes.ID,
	a probes.Args) probes.Result {
	return r.Run(ctx, id, a)
}

func (r *sizeCadenceRunner) measured() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sizes
}

// sizeMonitor is a monitor sampling every minute with sizes every every,
// through share (its clock is the share's).
func sizeMonitor(r ProbeRunner, every time.Duration, share *SizeShare) *Monitor {
	opts := testOptions()
	opts.Interval, opts.SizeInterval, opts.Sizes = time.Minute, every, share
	return &Monitor{runner: r, opts: opts, logFn: func(string, string, ...any) {},
		now: time.Now, last: map[seriesKey]lastPoint{}}
}

func readSize(t *testing.T, m *Monitor) Snapshot {
	t.Helper()
	snap, _ := m.read(context.Background())
	if snap.WAL == nil {
		t.Fatal("no WAL reading")
	}
	return snap
}

func TestSizeCadence_MeasuredAtMostEverySizeInterval(t *testing.T) {
	r := &sizeCadenceRunner{}
	share, clock := newTestShare()
	m := sizeMonitor(r, 10*time.Minute, share)
	var fresh []int
	for minute := 0; minute < 25; minute++ {
		snap := readSize(t, m)
		if snap.SizeFresh {
			fresh = append(fresh, minute)
		}
		if snap.WAL.DatabaseBytes <= 0 {
			t.Fatalf("minute %d: no size reading (%v) between measurements", minute,
				snap.WAL.DatabaseBytes)
		}
		clock.advance(time.Minute)
	}
	if r.measured() != 3 || len(fresh) != 3 || fresh[0] != 0 || fresh[1] != 10 ||
		fresh[2] != 20 {
		t.Fatalf("%d measurements, fresh at minutes %v; want 3 at 0, 10 and 20",
			r.measured(), fresh)
	}
}

// Boundaries: a reading exactly SizeInterval old is measured again; one
// a nanosecond younger is reused.
func TestSizeCadence_IntervalBoundary(t *testing.T) {
	r := &sizeCadenceRunner{}
	share, clock := newTestShare()
	m := sizeMonitor(r, 10*time.Minute, share)
	first := readSize(t, m)
	clock.advance(10*time.Minute - time.Nanosecond)
	reused := readSize(t, m)
	if reused.SizeFresh || reused.WAL.DatabaseBytes != first.WAL.DatabaseBytes ||
		r.measured() != 1 {
		t.Fatalf("just under the interval: fresh %v, %v bytes (first %v), %d measurements",
			reused.SizeFresh, reused.WAL.DatabaseBytes, first.WAL.DatabaseBytes,
			r.measured())
	}
	clock.advance(time.Nanosecond)
	again := readSize(t, m)
	if !again.SizeFresh || again.WAL.DatabaseBytes == first.WAL.DatabaseBytes ||
		r.measured() != 2 {
		t.Fatalf("at the interval: fresh %v, %v bytes, %d measurements", again.SizeFresh,
			again.WAL.DatabaseBytes, r.measured())
	}
}

// SizeInterval 0 keeps the old cadence: measured and sampled every tick
// (no share: every read).
func TestSizeCadence_ZeroIntervalMeasuresEveryTick(t *testing.T) {
	r := &sizeCadenceRunner{}
	m := sizeMonitor(r, 0, nil)
	for i := 0; i < 3; i++ {
		if snap := readSize(t, m); !snap.SizeFresh {
			t.Fatalf("read %d not fresh with a zero size interval", i)
		}
	}
	if r.measured() != 3 {
		t.Fatalf("%d measurements in 3 reads, want 3", r.measured())
	}
}

// A failed measurement is reported, samples nothing and is retried on
// the next tick (it is never reused).
func TestSizeCadence_FailedMeasurementIsRetriedNextTick(t *testing.T) {
	r := &sizeCadenceRunner{failSize: true}
	share, clock := newTestShare()
	m := sizeMonitor(r, 10*time.Minute, share)
	snap, errs := m.read(context.Background())
	if snap.SizeFresh || probes.Known(snap.WAL.DatabaseBytes) ||
		!strings.Contains(joinErrs(errs), string(probes.ClusterDatabaseSizeProbe)) {
		t.Fatalf("failed measurement: fresh %v, bytes %v, errors %v", snap.SizeFresh,
			snap.WAL.DatabaseBytes, errs)
	}
	r.mu.Lock()
	r.failSize = false
	r.mu.Unlock()
	clock.advance(time.Minute)
	if snap := readSize(t, m); !snap.SizeFresh || r.measured() != 3 {
		// the failed pass measured twice (one retry), this one once
		t.Fatalf("after a failure: fresh %v, %d measurements, want fresh after 3",
			snap.SizeFresh, r.measured())
	}
}

func joinErrs(errs []error) string {
	var parts []string
	for _, err := range errs {
		if err != nil {
			parts = append(parts, err.Error())
		}
	}
	return strings.Join(parts, "; ")
}

// Fleet runtimes on one cluster: one measurement per size interval for
// all of them, and each samples that measurement exactly once.
func TestSizeCadence_FleetRuntimesSampleTheSharedReadingOnce(t *testing.T) {
	r := &sizeCadenceRunner{}
	share, clock := newTestShare()
	a, b := sizeMonitor(r, 10*time.Minute, share), sizeMonitor(r, 10*time.Minute, share)
	freshA, freshB := 0, 0
	for minute := 0; minute < 20; minute++ {
		if readSize(t, a).SizeFresh {
			freshA++
		}
		if readSize(t, b).SizeFresh {
			freshB++
		}
		clock.advance(time.Minute)
	}
	if r.measured() != 2 || freshA != 2 || freshB != 2 {
		t.Fatalf("%d measurements, fresh %d and %d times; want 2 each", r.measured(),
			freshA, freshB)
	}
}

// Concurrent reads of one monitor: one measurement, sampled once.
func TestSizeCadence_ConcurrentReadsSampleOnce(t *testing.T) {
	r := &sizeCadenceRunner{}
	share, _ := newTestShare()
	m := sizeMonitor(r, 10*time.Minute, share)
	var wg sync.WaitGroup
	var mu sync.Mutex
	fresh := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, _ := m.read(context.Background())
			if snap.SizeFresh {
				mu.Lock()
				fresh++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if r.measured() != 1 || fresh != 1 {
		t.Fatalf("%d measurements, %d fresh reads; want 1 and 1", r.measured(), fresh)
	}
}

// A monitor without a share still honors its size interval.
func TestSizeCadence_NoShareKeepsItsOwnReading(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://sage@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	r := &sizeCadenceRunner{}
	opts := testOptions()
	opts.Interval, opts.SizeInterval, opts.Retention = time.Minute, 10*time.Minute,
		48*time.Hour
	m, err := NewMonitor(pool, r, nil, opts, nil)
	if err != nil {
		t.Fatalf("NewMonitor: %v", err)
	}
	for i := 0; i < 3; i++ {
		readSize(t, m)
	}
	if r.measured() != 1 {
		t.Fatalf("%d measurements in 3 reads without a share, want 1", r.measured())
	}
}

// Defaults: a 60 s tick and a 600 s size interval measure the cluster six
// times an hour, not sixty.
func TestSizeCadence_CallsPerHourAtDefaults(t *testing.T) {
	r := &sizeCadenceRunner{}
	share, clock := newTestShare()
	m := sizeMonitor(r, 600*time.Second, share)
	for tick := 0; tick < 60; tick++ {
		readSize(t, m)
		clock.advance(time.Minute)
	}
	if r.measured() != 6 {
		t.Fatalf("%d size measurements an hour at defaults, want 6", r.measured())
	}
}

// Only a fresh size is sampled; the WAL position is sampled every tick.
func TestBuildSamples_OnlyFreshSizesAreSampled(t *testing.T) {
	s := fullSnapshot()
	s.SizeFresh = false
	got := byKey(BuildSamples(s, Options{DiskCapacityBytes: 1e11}))
	for _, key := range []string{"database_bytes/cluster", "disk_used/cluster"} {
		if _, ok := got[key]; ok {
			t.Fatalf("a reused size was sampled as %s", key)
		}
	}
	if _, ok := got["wal_position/cluster"]; !ok {
		t.Fatalf("no wal_position sample beside a reused size: %v", got)
	}
	s.SizeFresh = true
	got = byKey(BuildSamples(s, Options{DiskCapacityBytes: 1e11}))
	for _, key := range []string{"database_bytes/cluster", "disk_used/cluster"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("a fresh size was not sampled as %s: %v", key, got)
		}
	}
}

func TestOptions_SizeIntervalValidation(t *testing.T) {
	cases := []struct {
		every time.Duration
		ok    bool
	}{
		{-time.Second, false}, {0, true}, {time.Second, false},
		{time.Minute - time.Nanosecond, false}, {time.Minute, true}, {time.Hour, true},
	}
	for _, c := range cases {
		opts := testOptions()
		opts.Interval, opts.Retention, opts.SizeInterval = time.Minute, 48*time.Hour,
			c.every
		err := opts.validate()
		if (err == nil) != c.ok {
			t.Errorf("size interval %s: err %v, want ok %v", c.every, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "size interval") {
			t.Errorf("size interval %s: error %q does not name it", c.every, err)
		}
	}
}
