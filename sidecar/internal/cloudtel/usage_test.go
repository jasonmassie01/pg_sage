package cloudtel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// market-04: Runtime rolls each good sample into the resource's hourly
// usage and hands the hour's aggregate to every attached database's sink.

// usageTable stands in for sage.host_usage_hourly: one row per (resource,
// hour), replaced by each write, as the SQL upsert does.
type usageTable struct {
	mu   sync.Mutex
	rows map[string]HourlyUsage
	n    int
}

func newUsageTable() *usageTable { return &usageTable{rows: map[string]HourlyUsage{}} }

func (u *usageTable) sink(_ context.Context, h HourlyUsage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rows[h.Resource+"|"+h.Hour.Format(time.RFC3339)] = h
	u.n++
	return nil
}

func (u *usageTable) only(t *testing.T) HourlyUsage {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.rows) != 1 {
		t.Fatalf("%d hourly rows, want 1: %+v", len(u.rows), u.rows)
	}
	for _, h := range u.rows {
		return h
	}
	return HourlyUsage{}
}

func sizedSample(at time.Time, cpu float64) Sample {
	s := healthySample(at)
	s.CPUPct = &Point{Value: cpu, At: at.Add(-time.Minute)}
	s.InstanceClass, s.VCPUs = "db.r6g.large", 2
	s.Connections = &Point{Value: cpu * 2, At: at.Add(-time.Minute)}
	s.DBLoad = &Point{Value: cpu / 10, At: at.Add(-time.Minute)}
	return s
}

func usageRuntime(t *testing.T, samples []Sample) (*Runtime, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 2, 10, 0, 30, 0, time.UTC)}
	return newTestRuntime(t, &stubSource{provider: "rds", samples: samples}, c), c
}

func TestUsageRollsAnHourIntoPercentiles(t *testing.T) {
	r, c := usageRuntime(t, nil)
	table := newUsageTable()
	detach := r.AttachUsage("orders", table.sink, nil)
	defer detach()
	for i := 1; i <= 20; i++ {
		freshPoll(t, r, c, float64(i))
		c.advance(2 * time.Minute)
	}
	h := table.only(t)
	if h.Resource != "rds:us-east-1/orders" || !h.Hour.Equal(
		time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)) || h.Samples != 20 {
		t.Fatalf("row = %+v, want resource/hour 10:00/20 samples", h)
	}
	wantF(t, "cpu p50", h.CPUP50, 10)
	wantF(t, "cpu p95", h.CPUP95, 19)
	wantF(t, "cpu max", h.CPUMax, 20)
	wantF(t, "connections max", h.ConnectionsMax, 40)
	wantF(t, "db load p95", h.DBLoadP95, 1.9)
	wantF(t, "mem available min", h.MemAvailableMin, 6*gib)
	if h.InstanceClass != "db.r6g.large" || h.VCPUs != 2 || h.MemoryTotalBytes != 16*gib ||
		h.AllocatedStorageBytes != 100*gib {
		t.Fatalf("size = %+v", h)
	}
}

func wantF(t *testing.T, label string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %v", label, want)
	}
	if d := *got - want; d > 1e-9 || d < -1e-9 {
		t.Fatalf("%s = %v, want %v", label, *got, want)
	}
}

func TestUsageNewHourStartsANewRow(t *testing.T) {
	r, c := usageRuntime(t, []Sample{sizedSample(time.Time{}, 30)})
	table := newUsageTable()
	r.AttachUsage("orders", table.sink, nil)
	freshPoll(t, r, c, 30)
	c.advance(time.Hour)
	freshPoll(t, r, c, 50)
	table.mu.Lock()
	defer table.mu.Unlock()
	if len(table.rows) != 2 {
		t.Fatalf("%d rows, want one per hour", len(table.rows))
	}
	for _, h := range table.rows {
		if h.Samples != 1 {
			t.Fatalf("row %v has %d samples, want 1 (the new hour starts empty)", h.Hour,
				h.Samples)
		}
	}
}

// freshPoll polls one sample whose points are fresh at the clock.
func freshPoll(t *testing.T, r *Runtime, c *clock, cpu float64) {
	t.Helper()
	s := sizedSample(c.now(), cpu)
	for _, p := range []*Point{s.CPUPct, s.Connections, s.DBLoad, s.MemoryTotalBytes,
		s.FreeableMemoryBytes, s.FreeStorageBytes, s.AllocatedStorageBytes,
		s.StorageCapacityBytes} {
		p.At = c.now()
	}
	r.src = &stubSource{provider: "rds", samples: []Sample{s}}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func TestUsageClassChangeEndsTheOldSizesWindow(t *testing.T) {
	r, c := usageRuntime(t, nil)
	var got []HourlyUsage
	r.AttachUsage("orders", func(_ context.Context, h HourlyUsage) error {
		got = append(got, h)
		return nil
	}, nil)
	freshPoll(t, r, c, 30)
	freshPoll(t, r, c, 40)
	c.advance(time.Minute)
	s := sizedSample(c.now(), 10)
	s.InstanceClass, s.VCPUs = "db.r6g.xlarge", 4
	for _, p := range []*Point{s.CPUPct, s.Connections, s.DBLoad, s.MemoryTotalBytes,
		s.FreeableMemoryBytes} {
		p.At = c.now()
	}
	r.src = &stubSource{provider: "rds", samples: []Sample{s}}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	last := got[len(got)-1]
	if last.InstanceClass != "db.r6g.xlarge" || last.VCPUs != 4 || last.Samples != 1 {
		t.Fatalf("after resize: %+v, want a new 1-sample window of the new class", last)
	}
	wantF(t, "cpu max after resize", last.CPUMax, 10)
}

func TestUsageWriteFailureIsReportedAndPollSucceeds(t *testing.T) {
	r, c := usageRuntime(t, nil)
	var reported []string
	r.AttachUsage("orders", func(context.Context, HourlyUsage) error {
		return errors.New("relation sage.host_usage_hourly does not exist")
	}, func(database string, h HourlyUsage, err error) {
		reported = append(reported, database+"|"+h.Resource+"|"+err.Error())
	})
	freshPoll(t, r, c, 30)
	if len(reported) != 1 || reported[0] !=
		"orders|rds:us-east-1/orders|relation sage.host_usage_hourly does not exist" {
		t.Fatalf("reported = %q, want the database, resource and error", reported)
	}
	if r.Resource() != "rds:us-east-1/orders" {
		t.Fatalf("Resource = %q after a good poll", r.Resource())
	}
	if cpu, err := r.CurrentCPU(context.Background()); err != nil || cpu != 30 {
		t.Fatalf("CurrentCPU = %v, %v: a failed usage write must not fail the poll", cpu, err)
	}
}

func TestUsageNoSinkNoAggregationWork(t *testing.T) {
	// Nil sinks: a runtime without attached databases still polls.
	r, c := usageRuntime(t, nil)
	freshPoll(t, r, c, 30)
	if r.AttachUsage("orders", nil, nil) == nil {
		t.Fatal("AttachUsage(nil) must return a no-op detach, not nil")
	}
}

func TestUsageDetachStopsWrites(t *testing.T) {
	r, c := usageRuntime(t, nil)
	table := newUsageTable()
	detach := r.AttachUsage("orders", table.sink, nil)
	freshPoll(t, r, c, 30)
	detach()
	detach() // idempotent
	freshPoll(t, r, c, 40)
	if table.n != 1 {
		t.Fatalf("%d writes, want 1 (none after detach)", table.n)
	}
}

func TestUsageStaleCPUIsNotAveragedIn(t *testing.T) {
	r, c := usageRuntime(t, nil)
	table := newUsageTable()
	r.AttachUsage("orders", table.sink, nil)
	s := sizedSample(c.now(), 99)
	s.CPUPct.At = c.now().Add(-time.Hour) // stale: unknown, never averaged
	r.src = &stubSource{provider: "rds", samples: []Sample{s}}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	h := table.only(t)
	if h.CPUP95 != nil || h.CPUMax != nil || h.Samples != 1 {
		t.Fatalf("row = %+v, want the sample counted with CPU unknown", h)
	}
}

// Concurrent access: two fleet databases on one instance share the
// runtime; concurrent polls roll into one hourly row per resource.
func TestUsageTwoDatabasesOneResourceOneRow(t *testing.T) {
	r, c := usageRuntime(t, nil)
	table := newUsageTable()
	r.AttachUsage("orders", table.sink, nil)
	r.AttachUsage("billing", table.sink, nil)
	s := sizedSample(c.now(), 25)
	for _, p := range []*Point{s.CPUPct, s.Connections, s.DBLoad, s.MemoryTotalBytes,
		s.FreeableMemoryBytes} {
		p.At = c.now()
	}
	r.src = &stubSource{provider: "rds", samples: []Sample{s}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Poll(context.Background()); err != nil {
				t.Errorf("poll: %v", err)
			}
		}()
	}
	wg.Wait()
	h := table.only(t)
	if h.Samples != 8 {
		t.Fatalf("samples = %d, want 8 (each poll counted once)", h.Samples)
	}
	if table.n != 16 {
		t.Fatalf("%d writes, want 8 polls x 2 databases", table.n)
	}
}
