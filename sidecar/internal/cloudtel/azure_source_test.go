package cloudtel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Golden: the Microsoft.Insights metric names read, verified on 2026-11-12
// against "Supported metrics - Microsoft.DBforPostgreSQL/flexibleServers"
// (there is no storage_limit metric: capacity is used + free).
func TestAzureMetricNamesGolden(t *testing.T) {
	want := []string{"cpu_percent", "memory_percent", "storage_used", "storage_free",
		"active_connections", "read_iops", "write_iops"}
	if strings.Join(azurePrimaryMetrics, ",") != strings.Join(want, ",") {
		t.Fatalf("azurePrimaryMetrics = %v, want %v", azurePrimaryMetrics, want)
	}
	if azureReplicaLagMetric != "physical_replication_delay_in_seconds" {
		t.Fatalf("replica lag metric = %q", azureReplicaLagMetric)
	}
}

func TestAzureCollectFillsCPUMemoryStorageLag(t *testing.T) {
	f := newFakeARM(t).healthy()
	s, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != "azure" || s.Resource != "azure:"+strings.ToLower(azureServer) {
		t.Fatalf("identity = %q %q", s.Provider, s.Resource)
	}
	if s.CPUPct == nil || s.CPUPct.Value != 31.5 ||
		!s.CPUPct.At.Equal(azureNow.Add(-2*time.Minute)) {
		t.Fatalf("cpu = %+v, want 31.5 at its provider timestamp", s.CPUPct)
	}
	if s.MemoryTotalBytes == nil || s.MemoryTotalBytes.Value != 16*gibF ||
		s.MemoryTotalSource != MemorySourceInstanceClass {
		t.Fatalf("memory total = %+v (%s), want D4ds_v4 = 16 GiB", s.MemoryTotalBytes,
			s.MemoryTotalSource)
	}
	if s.FreeableMemoryBytes == nil || s.FreeableMemoryBytes.Value != 4*gibF {
		t.Fatalf("available = %+v, want 25%% of 16 GiB", s.FreeableMemoryBytes)
	}
	if s.FreeStorageBytes == nil || s.FreeStorageBytes.Value != 32*gibF ||
		s.StorageCapacityBytes == nil || s.StorageCapacityBytes.Value != 128*gibF {
		t.Fatalf("storage = free %+v cap %+v", s.FreeStorageBytes, s.StorageCapacityBytes)
	}
	if free, capacity, ok := s.EffectiveFreeStorage(azureNow); !ok || free != 32*gibF ||
		capacity != 128*gibF {
		t.Fatalf("EffectiveFreeStorage = %v %v %t", free, capacity, ok)
	}
	if s.ReplicaLagSeconds == nil || s.ReplicaLagSeconds.Value != 8 {
		t.Fatalf("lag = %+v, want the replica's maximum (8 s)", s.ReplicaLagSeconds)
	}
	if s.Connections == nil || s.Connections.Value != 12 || s.ReadIOPS == nil ||
		s.ReadIOPS.Value != 150 || s.WriteIOPS == nil || s.WriteIOPS.Value != 80 {
		t.Fatalf("connections/iops = %+v %+v %+v", s.Connections, s.ReadIOPS, s.WriteIOPS)
	}
	if len(s.Missing) != 0 {
		t.Fatalf("missing = %v, want none", s.Missing)
	}
	for _, b := range f.bearers {
		if b != "Bearer eyJ.azure" {
			t.Fatalf("bearer = %q", b)
		}
	}
}

func TestAzureMetricsRequestNamesResourceAndWindow(t *testing.T) {
	f := newFakeARM(t).healthy()
	if _, err := f.source(t, nil, nil).Collect(context.Background(), azureNow); err != nil {
		t.Fatal(err)
	}
	var primary string
	for _, p := range f.paths {
		if strings.Contains(p, "/flexibleServers/orders/providers/Microsoft.Insights/metrics") {
			primary = p
		}
	}
	for _, want := range []string{"metricnames=cpu_percent%2Cmemory_percent",
		"interval=PT1M", "aggregation=Average%2CMaximum", "timespan=2026-11-12T14%3A45%3A00Z"} {
		if !strings.Contains(primary, want) {
			t.Errorf("metrics request %q lacks %q", primary, want)
		}
	}
}

func TestAzureUnknownSKULeavesMemoryTotalNilWithReason(t *testing.T) {
	f := newFakeARM(t).healthy()
	f.sku = "Standard_Z9future_v9"
	s, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemoryTotalBytes != nil || s.FreeableMemoryBytes != nil {
		t.Fatalf("unknown SKU: total %+v available %+v, want both unknown",
			s.MemoryTotalBytes, s.FreeableMemoryBytes)
	}
	if !missingMentions(s, "memory_total") ||
		!strings.Contains(strings.Join(s.Missing, ";"), "Standard_Z9future_v9") {
		t.Fatalf("missing = %v, want a memory_total reason naming the SKU", s.Missing)
	}
}

func TestAzureMalformedJSONIsTyped(t *testing.T) {
	for _, key := range []string{"server", "replicas", "metrics:orders"} {
		f := newFakeARM(t).healthy()
		f.body[key] = `{"value": [`
		_, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", key, err)
		}
	}
}

func TestAzureEmptyTimeseriesIsUnavailableNotZero(t *testing.T) {
	f := newFakeARM(t)
	f.metrics["orders"] = map[string][]agoPoint{}
	s, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUPct != nil || s.FreeableMemoryBytes != nil || s.FreeStorageBytes != nil ||
		s.Connections != nil || s.ReplicaLagSeconds != nil {
		t.Fatalf("empty timeseries produced values: %+v", s)
	}
	if _, err := s.CPU(azureNow); err == nil {
		t.Fatal("CPU() with an empty timeseries must be unavailable, not 0%")
	}
	for _, want := range []string{"cpu", "freeable_memory", "free_storage"} {
		if !missingMentions(s, want) {
			t.Errorf("missing = %v, want a %s reason", s.Missing, want)
		}
	}
}

func TestAzureForbiddenNamesTheMissingRole(t *testing.T) {
	cases := []struct{ key, role string }{
		{"server", "Reader"}, {"metrics:orders", "Monitoring Reader"},
	}
	for _, tc := range cases {
		f := newFakeARM(t).healthy()
		f.fail[tc.key] = []int{403}
		_, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
		if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), tc.role) {
			t.Errorf("%s 403: err = %v, want ErrAuth naming %q", tc.key, err, tc.role)
		}
	}
	f := newFakeARM(t).healthy()
	f.fail["server"] = []int{404}
	if _, err := f.source(t, nil, nil).Collect(context.Background(),
		azureNow); !errors.Is(err, ErrIdentity) {
		t.Fatalf("404 err = %v, want ErrIdentity", err)
	}
	f = newFakeARM(t).healthy()
	f.fail["server"] = []int{500}
	if _, err := f.source(t, nil, nil).Collect(context.Background(),
		azureNow); !errors.Is(err, ErrProvider) {
		t.Fatalf("500 err = %v, want ErrProvider", err)
	}
}

func TestAzureThrottledIsRetriedWithBackoff(t *testing.T) {
	f := newFakeARM(t).healthy()
	sleep := &recordSleep{}
	f.fail["server"] = []int{429, 429}
	s, err := f.source(t, nil, sleep).Collect(context.Background(), azureNow)
	if err != nil || s.CPUPct == nil {
		t.Fatalf("two 429s then success: %v %+v", err, s.CPUPct)
	}
	if len(sleep.waits) != 2 || sleep.waits[1] <= sleep.waits[0] {
		t.Fatalf("waits = %v, want two growing backoffs", sleep.waits)
	}
	f.fail["server"] = []int{429, 429, 429}
	if _, err := f.source(t, nil, sleep).Collect(context.Background(),
		azureNow); !errors.Is(err, ErrThrottled) {
		t.Fatalf("persistent 429: err = %v, want ErrThrottled", err)
	}
}

func TestAzureTokenFailureIsNoCredentials(t *testing.T) {
	f := newFakeARM(t).healthy()
	_, err := f.source(t, &azureTokens{err: errAzureLogin}, nil).Collect(
		context.Background(), azureNow)
	if !errors.Is(err, ErrNoCredentials) || !strings.Contains(err.Error(), "azure") {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	if len(f.paths) != 0 {
		t.Fatalf("requests without a token: %v", f.paths)
	}
}

func TestAzureCPUBoundaryAndFreshness(t *testing.T) {
	cases := []struct {
		name    string
		value   float64
		ago     time.Duration
		wantCPU bool
	}{
		{"exactly 100", 100, time.Minute, true},
		{"just above 100", 100.01, time.Minute, false},
		{"negative", -1, time.Minute, false},
		{"exactly MaxPointAge old", 50, MaxPointAge, true},
		{"one second staler", 50, MaxPointAge + time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeARM(t).healthy()
			f.metrics["orders"]["cpu_percent"] = []agoPoint{{value: tc.value, ago: tc.ago}}
			s, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CPU(azureNow); (err == nil) != tc.wantCPU {
				t.Fatalf("CPU() err = %v, want available=%t (%+v)", err, tc.wantCPU, s.CPUPct)
			}
		})
	}
}

func TestAzureAutoGrowStorageIsUnbounded(t *testing.T) {
	f := newFakeARM(t).healthy()
	f.autoGrow = "Enabled"
	s, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
	if err != nil {
		t.Fatal(err)
	}
	if !s.StorageAutoGrows {
		t.Fatal("autoGrow Enabled must mark storage as auto-growing")
	}
	if _, _, ok := s.EffectiveFreeStorage(azureNow); ok {
		t.Fatal("auto-growing storage has no bounded capacity")
	}
}

func TestAzureFetchesPrimaryAndReplicasInParallel(t *testing.T) {
	f := newFakeARM(t).healthy()
	f.replicas = []string{"orders-ro1", "orders-ro2"}
	f.metrics["orders-ro2"] = map[string][]agoPoint{
		"physical_replication_delay_in_seconds": {{value: 40, ago: time.Minute}}}
	f.delay = 30 * time.Millisecond
	s, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
	if err != nil {
		t.Fatal(err)
	}
	if f.maxConc < 2 {
		t.Fatalf("max concurrent requests = %d, want parallel metric fetches", f.maxConc)
	}
	if s.ReplicaLagSeconds == nil || s.ReplicaLagSeconds.Value != 41 {
		t.Fatalf("lag = %+v, want the worst replica (41 s)", s.ReplicaLagSeconds)
	}
}

func TestAzureReplicaWithoutLagIsMissing(t *testing.T) {
	f := newFakeARM(t).healthy()
	f.metrics["orders-ro1"] = map[string][]agoPoint{}
	s, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.ReplicaLagSeconds != nil || !missingMentions(s, "replica_lag") {
		t.Fatalf("lag %+v missing %v: a silent replica is unknown lag", s.ReplicaLagSeconds,
			s.Missing)
	}
}

func TestNewAzureSourceValidatesIdentity(t *testing.T) {
	base := AzureOptions{SubscriptionID: azureSub, ResourceGroup: "rg-shop", Server: "orders",
		Token: (&azureTokens{}).token}
	cases := []struct {
		name   string
		mutate func(*AzureOptions)
		want   error
	}{
		{"bad subscription", func(o *AzureOptions) { o.SubscriptionID = "prod" }, ErrIdentity},
		{"bad group", func(o *AzureOptions) { o.ResourceGroup = "rg/../x" }, ErrIdentity},
		{"bad server", func(o *AzureOptions) { o.Server = "Orders_1" }, ErrIdentity},
		{"empty server", func(o *AzureOptions) { o.Server = "" }, ErrIdentity},
		{"no token", func(o *AzureOptions) { o.Token = nil }, ErrNoCredentials},
	}
	for _, tc := range cases {
		o := base
		tc.mutate(&o)
		if _, err := NewAzureSource(o); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestAzureSKUMemoryTable(t *testing.T) {
	for sku, gib := range map[string]float64{
		"Standard_B1ms": 2, "Standard_B2s": 4, "Standard_B2ms": 8, "Standard_B20ms": 80,
		"Standard_D2s_v3": 8, "Standard_D4ds_v4": 16, "Standard_D96ds_v5": 384,
		"Standard_D192ds_v6": 768, "Standard_E2ds_v4": 16, "Standard_E20ds_v5": 160,
		"Standard_E48ads_v6": 384, "Standard_E64s_v3": 432, "Standard_E64ds_v5": 512,
		"Standard_E96ds_v6": 768, "Standard_E96ads_v5": 672, "Standard_E192ds_v6": 1832,
		"standard_d4ds_v4": 16,
		"Standard_D3ds_v4": 0, "Standard_E20s_v3": 0, "Standard_B3ms": 0, "": 0,
		"Standard_D4ds_v9": 0, "GP_Gen5_4": 0,
	} {
		if got := AzureSKUMemoryBytes(sku); got != gib*gibF {
			t.Errorf("AzureSKUMemoryBytes(%q) = %v GiB, want %v", sku, got/gibF, gib)
		}
	}
}

// State transitions: available, then 403, then unavailable, then a
// recovered credential makes it available again (through the Runtime).
func TestAzureRuntimeRecoversAfterAccessIsRestored(t *testing.T) {
	f := newFakeARM(t).healthy()
	tokens := &azureTokens{}
	rt, err := NewRuntime(f.source(t, tokens, nil), RuntimeOptions{Database: "orders",
		Now: func() time.Time { return azureNow }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rt.Poll(ctx); err != nil || !rt.Status().Available {
		t.Fatalf("first poll: %v %+v", err, rt.Status())
	}
	f.fail["server"] = []int{403}
	if err := rt.Poll(ctx); !errors.Is(err, ErrAuth) {
		t.Fatalf("403 poll err = %v", err)
	}
	st := rt.Status()
	if st.Available || !strings.Contains(st.Reason, "Reader") {
		t.Fatalf("after 403: %+v, want unavailable naming the role", st)
	}
	if _, err := rt.CurrentCPU(ctx); err == nil {
		t.Fatal("CPU must be unknown after a denied poll")
	}
	tokens.err = errAzureLogin
	if err := rt.Poll(ctx); !errors.Is(err, ErrNoCredentials) || rt.Status().Available {
		t.Fatalf("lost credential: %v %+v", err, rt.Status())
	}
	tokens.err = nil
	if err := rt.Poll(ctx); err != nil || !rt.Status().Available {
		t.Fatalf("recovered: %v %+v", err, rt.Status())
	}
	if cpu, err := rt.CurrentCPU(ctx); err != nil || cpu != 31.5 {
		t.Fatalf("recovered CPU = %v, %v", cpu, err)
	}
}
