package cloudtel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// stubSource replays samples and errors in order (the last repeats).
type stubSource struct {
	mu       sync.Mutex
	provider string
	samples  []Sample
	errs     []error
	calls    int
}

func (s *stubSource) Provider() string { return s.provider }

func (s *stubSource) Collect(ctx context.Context, now time.Time) (Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if err := ctx.Err(); err != nil {
		return Sample{}, err
	}
	var err error
	if len(s.errs) > 0 {
		err = s.errs[min(i, len(s.errs)-1)]
	}
	if err != nil {
		return Sample{}, err
	}
	sample := s.samples[min(i, len(s.samples)-1)]
	sample.CollectedAt = now
	return sample, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func healthySample(at time.Time) Sample {
	p := func(v float64) *Point { return &Point{Value: v, At: at.Add(-time.Minute)} }
	return Sample{Provider: "rds", Resource: "rds:us-east-1/orders", CPUPct: p(20),
		MemoryTotalBytes: p(16 * gib), FreeableMemoryBytes: p(6 * gib),
		FreeStorageBytes: p(60 * gib), AllocatedStorageBytes: p(100 * gib),
		StorageCapacityBytes: p(100 * gib), ReplicaLagSeconds: p(2),
		MemoryTotalSource: MemorySourceInstanceClass}
}

func newTestRuntime(t *testing.T, src Source, c *clock) *Runtime {
	t.Helper()
	r, err := NewRuntime(src, RuntimeOptions{Database: "orders", Limits: DefaultLimits(),
		Now: c.now})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return r
}

func TestRuntimePollPublishesEvidence(t *testing.T) {
	c := &clock{t: guardNow}
	r := newTestRuntime(t, &stubSource{provider: "rds",
		samples: []Sample{healthySample(guardNow)}}, c)
	if _, err := r.CurrentCPU(context.Background()); !errors.Is(err,
		verify.ErrLoadTelemetryUnavailable) {
		t.Fatalf("before the first poll CPU err = %v", err)
	}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	cpu, err := r.CurrentCPU(context.Background())
	if err != nil || cpu != 20 {
		t.Fatalf("CurrentCPU = %v, %v", cpu, err)
	}
	if total, avail := r.HostMemory(); total != int64(16*gib) || avail != int64(6*gib) {
		t.Fatalf("HostMemory = %d, %d", total, avail)
	}
	capBytes, err := r.CapacityBytes(context.Background())
	if err != nil || capBytes != int64(100*gib) {
		t.Fatalf("CapacityBytes = %d, %v", capBytes, err)
	}
	if w := r.HostWithhold(context.Background()); len(w) != 0 {
		t.Fatalf("healthy withhold = %v", w)
	}
	st := r.Status()
	if !st.Available || st.Reason != "" || st.Provider != "rds" || st.Database != "orders" ||
		st.Sample == nil || st.LastSuccessAt == nil || st.Resource != "rds:us-east-1/orders" {
		t.Fatalf("status = %+v", st)
	}
}

// A failed poll fails closed: the previous sample is dropped, CPU and
// memory become unknown, and the status says why.
func TestRuntimePollErrorInvalidates(t *testing.T) {
	c := &clock{t: guardNow}
	src := &stubSource{provider: "rds", samples: []Sample{healthySample(guardNow)},
		errs: []error{nil, fmt.Errorf("%w: CloudWatch Rate exceeded", ErrThrottled)}}
	r := newTestRuntime(t, src, c)
	if err := r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Poll(context.Background()); !errors.Is(err, ErrThrottled) {
		t.Fatalf("second poll err = %v", err)
	}
	if _, err := r.CurrentCPU(context.Background()); err == nil {
		t.Fatal("CPU must be unavailable after a failed poll")
	}
	if total, _ := r.HostMemory(); total != 0 {
		t.Fatalf("memory after failure = %d", total)
	}
	st := r.Status()
	if st.Available || !strings.HasPrefix(st.Reason, "unavailable: ") ||
		!strings.Contains(st.Reason, "throttled") || st.Sample != nil {
		t.Fatalf("status after failure = %+v", st)
	}
}

func TestRuntimeEvidenceGoesStaleWithoutPolls(t *testing.T) {
	c := &clock{t: guardNow}
	r := newTestRuntime(t, &stubSource{provider: "rds",
		samples: []Sample{healthySample(guardNow)}}, c)
	if err := r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.advance(MaxPointAge)
	if _, err := r.CurrentCPU(context.Background()); err == nil {
		t.Fatal("CPU older than MaxPointAge must be unavailable")
	}
	if total, _ := r.HostMemory(); total != 0 {
		t.Fatal("stale memory must be unknown")
	}
}

func TestUnavailableRuntimeReportsReason(t *testing.T) {
	r := Unavailable("orders", "cloud-sql", "set cloud_telemetry.gcp.project")
	if err := r.Poll(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Poll err = %v", err)
	}
	if _, err := r.CurrentCPU(context.Background()); !errors.Is(err, ErrUnavailable) ||
		!errors.Is(err, verify.ErrLoadTelemetryUnavailable) {
		t.Fatalf("CurrentCPU err = %v", err)
	}
	if _, err := r.CapacityBytes(context.Background()); err == nil {
		t.Fatal("unavailable capacity must error")
	}
	st := r.Status()
	if st.Available || st.Reason != "unavailable: set cloud_telemetry.gcp.project" ||
		st.Provider != "cloud-sql" {
		t.Fatalf("status = %+v", st)
	}
	if w := r.HostWithhold(context.Background()); len(w) != 0 {
		t.Fatalf("unavailable telemetry must not withhold (status quo): %v", w)
	}
}

func TestNewRuntimeValidates(t *testing.T) {
	if _, err := NewRuntime(nil, RuntimeOptions{Database: "x"}); err == nil {
		t.Fatal("nil source accepted")
	}
	src := &stubSource{provider: "rds", samples: []Sample{{}}}
	if _, err := NewRuntime(src, RuntimeOptions{}); err == nil {
		t.Fatal("empty database accepted")
	}
	if _, err := NewRuntime(src, RuntimeOptions{Database: "x",
		Limits: Limits{MaxReplicaLagSeconds: -1}}); err == nil {
		t.Fatal("invalid limits accepted")
	}
	r, err := NewRuntime(src, RuntimeOptions{Database: "x"})
	if err != nil || r.Interval() != DefaultInterval {
		t.Fatalf("default interval = %v, %v", r.Interval(), err)
	}
}

// Storage runway: the runtime keeps the effective free storage history,
// withholds when it runs out within the floor, and restarts the history
// when the capacity changes (a resize is not consumption).
func TestRuntimeStorageRunwayWithholds(t *testing.T) {
	c := &clock{t: guardNow}
	src := &stubSource{provider: "rds"}
	r := newTestRuntime(t, src, c)
	free := 50 * gib
	for i := 0; i < 12; i++ { // falls 4 GiB per 5 minutes: ~1h runway
		s := healthySample(c.now())
		s.FreeStorageBytes = &Point{Value: free, At: c.now()}
		src.mu.Lock()
		src.samples = []Sample{s}
		src.calls = 0
		src.mu.Unlock()
		if err := r.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		free -= 4 * gib
		c.advance(5 * time.Minute)
	}
	c.advance(-5 * time.Minute)
	st := r.Status()
	if !st.Runway.Known || st.Runway.Hours > 2 {
		t.Fatalf("runway = %+v", st.Runway)
	}
	w := r.HostWithhold(context.Background())
	if len(w) == 0 || !strings.Contains(strings.Join(w, ";"), "storage runs out") {
		t.Fatalf("withhold = %v", w)
	}
	resized := healthySample(c.now())
	resized.StorageCapacityBytes = &Point{Value: 400 * gib, At: c.now()}
	resized.AllocatedStorageBytes = &Point{Value: 400 * gib, At: c.now()}
	src.mu.Lock()
	src.samples, src.calls = []Sample{resized}, 0
	src.mu.Unlock()
	if err := r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.Runway.Known || st.Runway.Points != 1 {
		t.Fatalf("history must restart on resize: %+v", st.Runway)
	}
}

// Status hands out a copy: a caller mutating it cannot change the
// evidence the guards read.
func TestRuntimeStatusIsACopy(t *testing.T) {
	c := &clock{t: guardNow}
	r := newTestRuntime(t, &stubSource{provider: "rds",
		samples: []Sample{healthySample(guardNow)}}, c)
	if err := r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := r.Status()
	st.Sample.CPUPct.Value = 99
	st.Sample.Missing = append(st.Sample.Missing, "mutated")
	st.Sample.DBLoadByWait = map[string]float64{"x": 1}
	again := r.Status()
	if again.Sample.CPUPct.Value != 20 || len(again.Sample.Missing) != 0 ||
		again.Sample.DBLoadByWait != nil {
		t.Fatalf("Status leaked the live sample: %+v", again.Sample)
	}
	if cpu, _ := r.CurrentCPU(context.Background()); cpu != 20 {
		t.Fatalf("guard CPU changed through a status copy: %v", cpu)
	}
}

// Concurrent readers (executor admission, tuning, API) and the poller.
func TestRuntimeConcurrentAccess(t *testing.T) {
	c := &clock{t: guardNow}
	r := newTestRuntime(t, &stubSource{provider: "rds",
		samples: []Sample{healthySample(guardNow)}}, c)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = r.Poll(context.Background())
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = r.CurrentCPU(context.Background())
				_ = r.HostWithhold(context.Background())
				_, _ = r.HostMemory()
				st := r.Status()
				if st.Sample != nil {
					st.Sample.Missing = append(st.Sample.Missing, "mutated copy")
				}
			}
		}()
	}
	wg.Wait()
	if st := r.Status(); st.Sample == nil || len(st.Sample.Missing) != 0 {
		t.Fatalf("Status must return a copy: %+v", st.Sample)
	}
}

func TestRuntimeRunStopsOnCancel(t *testing.T) {
	c := &clock{t: guardNow}
	src := &stubSource{provider: "rds", samples: []Sample{healthySample(guardNow)}}
	r, err := NewRuntime(src, RuntimeOptions{Database: "orders", Now: c.now,
		Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx, nil); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	src.mu.Lock()
	calls := src.calls
	src.mu.Unlock()
	if calls < 2 {
		t.Fatalf("Run polled %d times", calls)
	}
	if _, err := r.CurrentCPU(context.Background()); err == nil {
		t.Fatal("evidence must be invalidated when Run stops")
	}
}

func TestRegistry(t *testing.T) {
	a := Unavailable("reg-a", "rds", "x")
	b := Unavailable("reg-b", "cloud-sql", "y")
	Register(a)
	Register(b)
	t.Cleanup(func() { Unregister(a); Unregister(b) })
	if Lookup("reg-a") != a || Lookup("reg-b") != b || Lookup("nope") != nil {
		t.Fatal("lookup mismatch")
	}
	other := Unavailable("reg-a", "rds", "z")
	Unregister(other) // a different runtime with the same name must not evict a
	if Lookup("reg-a") != a {
		t.Fatal("Unregister evicted another runtime")
	}
	names := []string{}
	for _, st := range Statuses() {
		if strings.HasPrefix(st.Database, "reg-") {
			names = append(names, st.Database)
		}
	}
	if strings.Join(names, ",") != "reg-a,reg-b" {
		t.Fatalf("statuses = %v (sorted by database)", names)
	}
}
