package cloudtel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Golden: the AlloyDB metric types read, verified on 2026-11-12 against
// Google's "Google Cloud metrics: A-B" list (alloydb section). A rename
// upstream must be a deliberate change here.
func TestAlloyDBMetricTypesGolden(t *testing.T) {
	want := []string{
		"alloydb.googleapis.com/instance/cpu/average_utilization",
		"alloydb.googleapis.com/instance/cpu/vcpus",
		"alloydb.googleapis.com/instance/memory/min_available_memory",
		"alloydb.googleapis.com/instance/postgres/total_connections",
		"alloydb.googleapis.com/instance/postgres/replication/maximum_lag",
	}
	if len(alloyDBMetrics) != len(want) {
		t.Fatalf("alloyDBMetrics = %v, want %d metrics", alloyDBMetrics, len(want))
	}
	for i, m := range alloyDBMetrics {
		if alloyDBMetric+m != want[i] {
			t.Errorf("metric %d = %q, want %q", i, alloyDBMetric+m, want[i])
		}
	}
}

func TestAlloyDBCollectFillsCPUMemoryLagWithProviderTimes(t *testing.T) {
	f := newFakeAlloyDB(t).healthy()
	s, err := f.source(t, nil).Collect(context.Background(), alloyNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != "alloydb" ||
		s.Resource != "alloydb:projects/shop-prod/clusters/orders/instances/orders-primary" {
		t.Fatalf("identity = %q %q", s.Provider, s.Resource)
	}
	if s.CPUPct == nil || s.CPUPct.Value != 42.5 ||
		!s.CPUPct.At.Equal(alloyNow.Add(-2*time.Minute)) {
		t.Fatalf("cpu = %+v, want the newest point 42.5 at its provider time", s.CPUPct)
	}
	if s.MemoryTotalBytes == nil || s.MemoryTotalBytes.Value != 32*gibF ||
		s.MemoryTotalSource != MemorySourceMachineTier {
		t.Fatalf("memory total = %+v (%s), want 4 vCPU = 32 GiB", s.MemoryTotalBytes,
			s.MemoryTotalSource)
	}
	if s.FreeableMemoryBytes == nil || s.FreeableMemoryBytes.Value != 8*gibF ||
		!s.FreeableMemoryBytes.At.Equal(alloyNow.Add(-3*time.Minute)) {
		t.Fatalf("available memory = %+v", s.FreeableMemoryBytes)
	}
	if s.ReplicaLagSeconds == nil || s.ReplicaLagSeconds.Value != 4.2 {
		t.Fatalf("lag = %+v, want the worst read pool lag 4200 ms = 4.2 s",
			s.ReplicaLagSeconds)
	}
	if s.Connections == nil || s.Connections.Value != 57 {
		t.Fatalf("connections = %+v", s.Connections)
	}
	if !s.StorageAutoGrows || s.FreeStorageBytes != nil {
		t.Fatalf("AlloyDB storage must auto-grow without a capacity: %+v", s)
	}
	if cpu, err := s.CPU(alloyNow); err != nil || cpu != 42.5 {
		t.Fatalf("CPU() = %v, %v", cpu, err)
	}
	if total, avail := s.HostMemory(alloyNow); total != int64(32*gibF) ||
		avail != int64(8*gibF) {
		t.Fatalf("HostMemory = %d, %d", total, avail)
	}
}

func TestAlloyDBFiltersNameTheInstanceResource(t *testing.T) {
	f := newFakeAlloyDB(t).healthy()
	if _, err := f.source(t, nil).Collect(context.Background(), alloyNow); err != nil {
		t.Fatal(err)
	}
	if len(f.filters) != len(alloyDBMetrics) {
		t.Fatalf("requests = %d, want one per metric", len(f.filters))
	}
	for _, filter := range f.filters {
		for _, want := range []string{`resource.type = "alloydb.googleapis.com/Instance"`,
			`resource.labels.cluster_id = "orders"`,
			`resource.labels.instance_id = "orders-primary"`} {
			if !strings.Contains(filter, want) {
				t.Errorf("filter %q lacks %q", filter, want)
			}
		}
	}
	for _, b := range f.bearers {
		if b != "Bearer ya29.alloy" {
			t.Fatalf("bearer = %q", b)
		}
	}
}

func TestAlloyDBUnknownVCPUsLeaveMemoryTotalNilWithReason(t *testing.T) {
	for _, vcpus := range []float64{1, 3, 128, 0} {
		f := newFakeAlloyDB(t).healthy()
		f.series["cpu/vcpus"] = []alloySeries{{int64v: true,
			points: []agoPoint{{value: vcpus, ago: time.Minute}}}}
		s, err := f.source(t, nil).Collect(context.Background(), alloyNow)
		if err != nil {
			t.Fatal(err)
		}
		if s.MemoryTotalBytes != nil {
			t.Fatalf("vcpus %v: memory total = %+v, want unknown", vcpus, s.MemoryTotalBytes)
		}
		if !missingMentions(s, "memory_total") {
			t.Fatalf("vcpus %v: missing = %v, want a memory_total reason", vcpus, s.Missing)
		}
		if total, _ := s.HostMemory(alloyNow); total != 0 {
			t.Fatalf("vcpus %v: HostMemory total = %d, want 0 (unknown)", vcpus, total)
		}
	}
}

func TestAlloyDBEmptyTimeseriesIsUnavailableNotZero(t *testing.T) {
	f := newFakeAlloyDB(t) // no series at all
	s, err := f.source(t, nil).Collect(context.Background(), alloyNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUPct != nil || s.FreeableMemoryBytes != nil || s.MemoryTotalBytes != nil ||
		s.ReplicaLagSeconds != nil || s.Connections != nil {
		t.Fatalf("empty series produced values: %+v", s)
	}
	if _, err := s.CPU(alloyNow); err == nil {
		t.Fatal("CPU() with no datapoint must be unavailable, not 0%")
	}
	if !missingMentions(s, "cpu") {
		t.Fatalf("missing = %v, want a cpu reason", s.Missing)
	}
}

func TestAlloyDBMalformedJSONIsTyped(t *testing.T) {
	f := newFakeAlloyDB(t)
	f.body = `{"timeSeries": [`
	_, err := f.source(t, nil).Collect(context.Background(), alloyNow)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

func TestAlloyDBForbiddenNamesTheMonitoringRole(t *testing.T) {
	f := newFakeAlloyDB(t).healthy()
	f.status = []int{403, 403, 403, 403, 403}
	_, err := f.source(t, nil).Collect(context.Background(), alloyNow)
	if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "roles/monitoring.viewer") {
		t.Fatalf("err = %v, want ErrAuth naming roles/monitoring.viewer", err)
	}
}

func TestAlloyDBThrottledIsRetriedWithBackoff(t *testing.T) {
	f := newFakeAlloyDB(t).healthy()
	f.delay = 0
	sleep := &recordSleep{}
	src := f.source(t, sleep)
	src.opts.Parallel = 1 // serial, so the queued 429s hit one metric
	f.status = []int{429, 429}
	s, err := src.Collect(context.Background(), alloyNow)
	if err != nil {
		t.Fatalf("two 429s then success: %v", err)
	}
	if s.CPUPct == nil {
		t.Fatal("retried metric lost its value")
	}
	if len(sleep.waits) != 2 || sleep.waits[1] <= sleep.waits[0] {
		t.Fatalf("waits = %v, want two growing backoffs", sleep.waits)
	}
	f.status = []int{429, 429, 429}
	_, err = src.Collect(context.Background(), alloyNow)
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("persistent 429: err = %v, want ErrThrottled after the retries", err)
	}
}

func TestAlloyDBThrottleRetryStopsAtTheCycleDeadline(t *testing.T) {
	f := newFakeAlloyDB(t).healthy()
	sleep := &recordSleep{}
	src := f.source(t, sleep)
	src.opts.Parallel = 1
	f.status = []int{429}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := src.Collect(ctx, alloyNow)
	if !errors.Is(err, ErrThrottled) || len(sleep.waits) != 0 {
		t.Fatalf("err = %v waits = %v: a backoff past the deadline must not be slept",
			err, sleep.waits)
	}
}

func TestAlloyDBCPUBoundaryAndFreshness(t *testing.T) {
	cases := []struct {
		name    string
		value   float64
		ago     time.Duration
		wantCPU bool
	}{
		{"exactly 100", 100, time.Minute, true},
		{"just above 100", 100.01, time.Minute, false},
		{"exactly MaxPointAge old", 50, MaxPointAge, true},
		{"one second staler", 50, MaxPointAge + time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAlloyDB(t).healthy()
			f.series["cpu/average_utilization"] = []alloySeries{{
				points: []agoPoint{{value: tc.value, ago: tc.ago}}}}
			s, err := f.source(t, nil).Collect(context.Background(), alloyNow)
			if err != nil {
				t.Fatal(err)
			}
			_, cpuErr := s.CPU(alloyNow)
			if (cpuErr == nil) != tc.wantCPU {
				t.Fatalf("CPU() err = %v, want available=%t (sample cpu %+v)", cpuErr,
					tc.wantCPU, s.CPUPct)
			}
		})
	}
}

func TestAlloyDBFetchesMetricsInParallel(t *testing.T) {
	f := newFakeAlloyDB(t).healthy()
	f.delay = 30 * time.Millisecond
	if _, err := f.source(t, nil).Collect(context.Background(), alloyNow); err != nil {
		t.Fatal(err)
	}
	if f.maxConc < 2 {
		t.Fatalf("max concurrent requests = %d, want the metrics fetched in parallel",
			f.maxConc)
	}
}

func TestNewAlloyDBSourceValidatesIdentity(t *testing.T) {
	base := AlloyDBOptions{Project: "shop-prod", Cluster: "orders", Instance: "primary",
		Token: staticToken("x")}
	cases := []struct {
		name   string
		mutate func(*AlloyDBOptions)
		want   error
	}{
		{"bad project", func(o *AlloyDBOptions) { o.Project = "X" }, ErrIdentity},
		{"no cluster", func(o *AlloyDBOptions) { o.Cluster = "" }, ErrIdentity},
		{"bad instance", func(o *AlloyDBOptions) { o.Instance = "a\"b" }, ErrIdentity},
		{"no token", func(o *AlloyDBOptions) { o.Token = nil }, ErrNoCredentials},
	}
	for _, tc := range cases {
		o := base
		tc.mutate(&o)
		if _, err := NewAlloyDBSource(o); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if src, err := NewAlloyDBSource(base); err != nil || src.Provider() != "alloydb" {
		t.Fatalf("valid identity: %v %v", src, err)
	}
}

func TestAlloyDBMemoryTable(t *testing.T) {
	for vcpus, gib := range map[float64]float64{2: 16, 4: 32, 8: 64, 16: 128, 32: 256,
		48: 384, 64: 512, 72: 576, 96: 768, 1: 0, 128: 0, 6: 0, -4: 0} {
		if got := AlloyDBMemoryBytes(vcpus); got != gib*gibF {
			t.Errorf("AlloyDBMemoryBytes(%v) = %v, want %v GiB", vcpus, got, gib)
		}
	}
}

func missingMentions(s Sample, prefix string) bool {
	for _, m := range s.Missing {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}
