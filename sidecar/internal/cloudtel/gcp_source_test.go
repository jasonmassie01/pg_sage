package cloudtel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

type failingToken struct{ err error }

func (f failingToken) Token(context.Context) (string, error) { return "", f.err }

// cloudSQLFixture is a 4 vCPU / 16 GB Cloud SQL instance with one replica.
func cloudSQLFixture(t *testing.T, autoResize bool, limitGB string) *fakeGCP {
	f := newFakeGCP(t, gcpNow)
	settings := map[string]any{"tier": "db-custom-4-16384", "dataDiskSizeGb": "200",
		"storageAutoResize": autoResize, "storageAutoResizeLimit": limitGB,
		"databaseFlags": []map[string]string{{"name": "work_mem", "value": "65536"},
			{"name": "log_min_duration_statement", "value": "500"}}}
	f.instances["main"] = map[string]any{"name": "main", "project": "proj-1",
		"region": "europe-west1", "databaseVersion": "POSTGRES_16",
		"instanceType": "CLOUD_SQL_INSTANCE", "replicaNames": []string{"main-replica"},
		"ipAddresses": []map[string]string{{"ipAddress": "10.20.0.5", "type": "PRIVATE"}},
		"settings":    settings}
	at := gcpNow.Add(-time.Minute)
	id := "proj-1:main"
	f.series = []gcpSeries{
		{metric: "cloudsql.googleapis.com/database/cpu/utilization", databaseID: id,
			points: []fakePoint{{at, 0.42}, {at.Add(-time.Minute), 0.99}}},
		{metric: "cloudsql.googleapis.com/database/memory/quota", databaseID: id,
			int64Value: true, points: []fakePoint{{at, 16 * gib}}},
		{metric: "cloudsql.googleapis.com/database/memory/usage", databaseID: id,
			int64Value: true, points: []fakePoint{{at, 10 * gib}}},
		{metric: "cloudsql.googleapis.com/database/disk/quota", databaseID: id,
			int64Value: true, points: []fakePoint{{at, 200 * gib}}},
		{metric: "cloudsql.googleapis.com/database/disk/bytes_used", databaseID: id,
			int64Value: true, points: []fakePoint{{at, 150 * gib}}},
		{metric: "cloudsql.googleapis.com/database/disk/read_ops_count", databaseID: id,
			int64Value: true, intervalSeconds: 60, points: []fakePoint{{at, 600}}},
		{metric: "cloudsql.googleapis.com/database/disk/write_ops_count", databaseID: id,
			int64Value: true, intervalSeconds: 60, points: []fakePoint{{at, 1200}}},
		{metric: "cloudsql.googleapis.com/database/postgresql/num_backends", databaseID: id,
			int64Value: true, labels: map[string]string{"database": "app"},
			points: []fakePoint{{at, 30}}},
		{metric: "cloudsql.googleapis.com/database/postgresql/num_backends", databaseID: id,
			int64Value: true, labels: map[string]string{"database": "postgres"},
			points: []fakePoint{{at, 4}}},
		{metric: "cloudsql.googleapis.com/database/replication/replica_lag",
			databaseID: "proj-1:main-replica", points: []fakePoint{{at, 7.5}}},
	}
	return f
}

func (f *fakeGCP) source(t *testing.T, opts GCPOptions) *GCPSource {
	t.Helper()
	if opts.Project == "" {
		opts.Project = "proj-1"
	}
	if opts.Token == nil {
		opts.Token = staticToken("ya29.static")
	}
	opts.MonitoringEndpoint, opts.SQLAdminEndpoint = f.srv.URL, f.srv.URL
	opts.HTTPClient = f.srv.Client()
	opts.Now = func() time.Time { return f.now }
	src, err := NewGCPSource(opts)
	if err != nil {
		t.Fatalf("NewGCPSource: %v", err)
	}
	return src
}

func TestGCPCollectCloudSQLHappyPath(t *testing.T) {
	f := cloudSQLFixture(t, false, "0")
	s, err := f.source(t, GCPOptions{Instance: "main"}).Collect(context.Background(), gcpNow)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if s.Provider != "cloud-sql" || s.Resource != "cloud-sql:proj-1:main" {
		t.Fatalf("provider/resource = %q/%q", s.Provider, s.Resource)
	}
	wantPoint(t, "cpu pct (fraction x100, newest)", s.CPUPct, 42)
	wantPoint(t, "memory total", s.MemoryTotalBytes, 16*gib)
	wantPoint(t, "memory available", s.FreeableMemoryBytes, 6*gib)
	wantPoint(t, "free storage", s.FreeStorageBytes, 50*gib)
	wantPoint(t, "allocated", s.AllocatedStorageBytes, 200*gib)
	wantPoint(t, "capacity", s.StorageCapacityBytes, 200*gib)
	wantPoint(t, "read iops", s.ReadIOPS, 10)
	wantPoint(t, "write iops", s.WriteIOPS, 20)
	wantPoint(t, "connections (all databases)", s.Connections, 34)
	wantPoint(t, "replica lag", s.ReplicaLagSeconds, 7.5)
	if s.MemoryTotalSource != MemorySourceCloudMonitoring || s.StorageAutoGrows {
		t.Fatalf("memsource=%q autogrows=%t", s.MemoryTotalSource, s.StorageAutoGrows)
	}
	for _, b := range f.bearers {
		if b != "Bearer ya29.static" {
			t.Fatalf("authorization = %q", b)
		}
	}
	if f.called("monitoring") != 1 {
		t.Fatalf("monitoring calls = %d, want one one_of query", f.called("monitoring"))
	}
	if !strings.Contains(f.filters[0], "one_of(") {
		t.Fatalf("filter = %q", f.filters[0])
	}
}

func TestGCPCollectAutoResizeStorage(t *testing.T) {
	f := cloudSQLFixture(t, true, "0")
	s, err := f.source(t, GCPOptions{Instance: "main"}).Collect(context.Background(), gcpNow)
	if err != nil {
		t.Fatal(err)
	}
	if !s.StorageAutoGrows || s.StorageCapacityBytes != nil {
		t.Fatalf("unlimited auto-resize must be unbounded: autogrows=%t cap=%v",
			s.StorageAutoGrows, s.StorageCapacityBytes)
	}
	g := cloudSQLFixture(t, true, "500")
	s, err = g.source(t, GCPOptions{Instance: "main"}).Collect(context.Background(), gcpNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.StorageAutoGrows {
		t.Fatal("a resize limit bounds the capacity")
	}
	wantPoint(t, "capacity = resize limit", s.StorageCapacityBytes, 500*gib)
}

func TestGCPCollectDiscoversInstanceByIP(t *testing.T) {
	f := cloudSQLFixture(t, false, "0")
	f.instances["other"] = map[string]any{"name": "other", "ipAddresses": []map[string]string{
		{"ipAddress": "10.20.0.9", "type": "PRIVATE"}}, "settings": map[string]any{}}
	s, err := f.source(t, GCPOptions{HostIP: "10.20.0.5"}).Collect(context.Background(), gcpNow)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if s.Resource != "cloud-sql:proj-1:main" {
		t.Fatalf("resource = %q", s.Resource)
	}
	_, err = f.source(t, GCPOptions{HostIP: "10.99.0.1"}).Collect(context.Background(), gcpNow)
	if !errors.Is(err, ErrIdentity) {
		t.Fatalf("unmatched IP err = %v, want ErrIdentity", err)
	}
}

func TestNewGCPSourceRejectsInvalidIdentity(t *testing.T) {
	cases := []GCPOptions{
		{Project: "", Instance: "main"},
		{Project: "proj-1"},
		{Project: "Proj_1", Instance: "main"},
		{Project: "proj-1", Instance: "main/../x"},
		{Project: "proj-1", HostIP: "db.example.com"},
	}
	for _, opts := range cases {
		opts.Token = staticToken("x")
		if _, err := NewGCPSource(opts); !errors.Is(err, ErrIdentity) {
			t.Errorf("NewGCPSource(%+v) err = %v, want ErrIdentity", opts, err)
		}
	}
	if _, err := NewGCPSource(GCPOptions{Project: "proj-1", Instance: "main"}); !errors.Is(
		err, ErrNoCredentials) {
		t.Errorf("nil token err = %v, want ErrNoCredentials", err)
	}
}

func TestGCPCollectErrorsAreDistinguishable(t *testing.T) {
	cases := []struct {
		name, key string
		fail      fakeFailure
		want      error
	}{
		{"monitoring throttled", "monitoring", fakeFailure{status: 429,
			code: "RESOURCE_EXHAUSTED", message: "quota"}, ErrThrottled},
		{"monitoring forbidden", "monitoring", fakeFailure{status: 403,
			code: "PERMISSION_DENIED", message: "monitoring.timeSeries.list"}, ErrAuth},
		{"monitoring unauthenticated", "monitoring", fakeFailure{status: 401,
			code: "UNAUTHENTICATED", message: "bad token"}, ErrAuth},
		{"monitoring malformed", "monitoring", fakeFailure{status: 200, body: "{"},
			ErrMalformed},
		{"monitoring 503", "monitoring", fakeFailure{status: 503, code: "UNAVAILABLE",
			message: "x"}, ErrProvider},
		{"sqladmin forbidden", "sqladmin", fakeFailure{status: 403,
			code: "PERMISSION_DENIED", message: "cloudsql.instances.get"}, ErrAuth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := cloudSQLFixture(t, false, "0")
			f.fail[tc.key] = tc.fail
			_, err := f.source(t, GCPOptions{Instance: "main"}).Collect(
				context.Background(), gcpNow)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	f := cloudSQLFixture(t, false, "0")
	_, err := f.source(t, GCPOptions{Instance: "gone"}).Collect(context.Background(), gcpNow)
	if !errors.Is(err, ErrIdentity) {
		t.Fatalf("missing instance err = %v, want ErrIdentity", err)
	}
	g := cloudSQLFixture(t, false, "0")
	_, err = g.source(t, GCPOptions{Instance: "main",
		Token: failingToken{err: ErrAuth}}).Collect(context.Background(), gcpNow)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("token failure err = %v, want ErrAuth", err)
	}
}

func TestGCPCollectPartialAndSkew(t *testing.T) {
	f := cloudSQLFixture(t, false, "0")
	f.series = f.series[:1] // CPU only
	s, err := f.source(t, GCPOptions{Instance: "main"}).Collect(context.Background(), gcpNow)
	if err != nil {
		t.Fatal(err)
	}
	wantPoint(t, "cpu", s.CPUPct, 42)
	for _, want := range []string{"freeable_memory", "free_storage", "replica_lag",
		"connections"} {
		if !containsPrefix(s.Missing, want) {
			t.Fatalf("missing = %v, want %s", s.Missing, want)
		}
	}
	if s.MemoryTotalBytes == nil || s.MemoryTotalSource != MemorySourceMachineTier {
		t.Fatalf("tier db-custom-4-16384 must ground memory: %v %q", s.MemoryTotalBytes,
			s.MemoryTotalSource)
	}
	wantPoint(t, "memory from tier", s.MemoryTotalBytes, 16384*1024*1024)

	g := cloudSQLFixture(t, false, "0")
	g.series[0].points = []fakePoint{{gcpNow.Add(15 * time.Minute), 0.5}}
	_, err = g.source(t, GCPOptions{Instance: "main"}).Collect(context.Background(), gcpNow)
	if !errors.Is(err, ErrClockSkew) {
		t.Fatalf("future point err = %v, want ErrClockSkew", err)
	}
	h := cloudSQLFixture(t, false, "0")
	h.dateSkew = -time.Hour
	_, err = h.source(t, GCPOptions{Instance: "main"}).Collect(context.Background(), gcpNow)
	if !errors.Is(err, ErrClockSkew) {
		t.Fatalf("date skew err = %v, want ErrClockSkew", err)
	}
	k := cloudSQLFixture(t, false, "0")
	k.series[0].points = []fakePoint{{gcpNow.Add(-time.Minute), 1.7}} // > 100%
	s, err = k.source(t, GCPOptions{Instance: "main"}).Collect(context.Background(), gcpNow)
	if err != nil || s.CPUPct != nil || !containsPrefix(s.Missing, "cpu") {
		t.Fatalf("invalid utilization must be missing: cpu=%v missing=%v err=%v",
			s.CPUPct, s.Missing, err)
	}
}

func TestGCPTargetReadsFlags(t *testing.T) {
	f := cloudSQLFixture(t, false, "0")
	target, err := f.source(t, GCPOptions{Instance: "main"}).Target(context.Background())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if target.Provider != "cloud-sql" || target.Project != "proj-1" ||
		target.InstanceID != "main" || !target.FlagsKnown ||
		target.Flags["work_mem"] != "65536" || len(target.Flags) != 2 {
		t.Fatalf("target = %+v", target)
	}
}
