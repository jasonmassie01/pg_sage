package cloudtel

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

var awsNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

const gib = float64(1 << 30)

// rdsFixture is a db.r6g.large (16 GiB) RDS PostgreSQL instance with two
// read replicas, storage autoscaling 100 -> 500 GiB, and PI enabled.
func rdsFixture(t *testing.T, pi bool) *fakeAWS {
	f := newFakeAWS(t, awsNow)
	f.instances["orders"] = instanceXML("orders", "db.r6g.large", "postgres",
		"orders-pg16", "in-sync", 100, 500, pi, "", "orders-r1", "orders-r2")
	f.metric("orders", "CPUUtilization", time.Minute, 37.5)
	f.metric("orders", "CPUUtilization", 2*time.Minute, 90) // older point is ignored
	f.metric("orders", "FreeableMemory", time.Minute, 4*gib)
	f.metric("orders", "FreeStorageSpace", time.Minute, 30*gib)
	f.metric("orders", "ReadIOPS", time.Minute, 120)
	f.metric("orders", "WriteIOPS", time.Minute, 340)
	f.metric("orders", "ReadThroughput", time.Minute, 5e6)
	f.metric("orders", "WriteThroughput", time.Minute, 7e6)
	f.metric("orders", "DatabaseConnections", time.Minute, 42)
	f.metric("orders-r1", "ReplicaLag", time.Minute, 3)
	f.metric("orders-r2", "ReplicaLag", time.Minute, 11)
	f.pi = []piSeries{
		{metric: "db.load.avg", points: []fakePoint{{awsNow.Add(-time.Minute), 1.6}}},
		{metric: "db.load.avg", wait: "CPU", points: []fakePoint{{awsNow.Add(-time.Minute), 1.2}}},
		{metric: "db.load.avg", wait: "IO", points: []fakePoint{{awsNow.Add(-time.Minute), 0.4}}},
		{metric: "os.memory.total.avg",
			points: []fakePoint{{awsNow.Add(-time.Minute), 16 * 1024 * 1024}}}, // KB
	}
	return f
}

func mustCollect(t *testing.T, src Source) Sample {
	t.Helper()
	s, err := src.Collect(context.Background(), awsNow)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return s
}

func wantPoint(t *testing.T, label string, p *Point, want float64) {
	t.Helper()
	if p == nil {
		t.Fatalf("%s = nil, want %v", label, want)
	}
	if math.Abs(p.Value-want) > 1e-6 {
		t.Fatalf("%s = %v, want %v", label, p.Value, want)
	}
}

func TestAWSCollectRDSHappyPath(t *testing.T) {
	f := rdsFixture(t, true)
	s := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"}))
	if s.Provider != "rds" || s.Resource != "rds:us-east-1/orders" {
		t.Fatalf("provider/resource = %q/%q", s.Provider, s.Resource)
	}
	wantPoint(t, "cpu", s.CPUPct, 37.5)
	wantPoint(t, "freeable", s.FreeableMemoryBytes, 4*gib)
	wantPoint(t, "free storage", s.FreeStorageBytes, 30*gib)
	wantPoint(t, "allocated", s.AllocatedStorageBytes, 100*gib)
	wantPoint(t, "capacity (autoscaling max)", s.StorageCapacityBytes, 500*gib)
	wantPoint(t, "read iops", s.ReadIOPS, 120)
	wantPoint(t, "write iops", s.WriteIOPS, 340)
	wantPoint(t, "read bps", s.ReadBytesPerSec, 5e6)
	wantPoint(t, "write bps", s.WriteBytesPerSec, 7e6)
	wantPoint(t, "connections", s.Connections, 42)
	wantPoint(t, "replica lag (max of replicas)", s.ReplicaLagSeconds, 11)
	wantPoint(t, "db load", s.DBLoad, 1.6)
	wantPoint(t, "memory total", s.MemoryTotalBytes, 16*gib)
	if s.MemoryTotalSource != MemorySourcePerformanceInsights {
		t.Fatalf("memory source = %q", s.MemoryTotalSource)
	}
	if s.DBLoadByWait["CPU"] != 1.2 || s.DBLoadByWait["IO"] != 0.4 || len(s.DBLoadByWait) != 2 {
		t.Fatalf("load by wait = %v", s.DBLoadByWait)
	}
	if s.StorageAutoGrows || len(s.Missing) != 0 {
		t.Fatalf("autogrows=%t missing=%v", s.StorageAutoGrows, s.Missing)
	}
	if !s.CPUPct.At.Equal(awsNow.Add(-time.Minute)) {
		t.Fatalf("cpu at = %v, want the newest datapoint", s.CPUPct.At)
	}
	assertSigned(t, f)
	if len(f.piRequests) != 1 || f.piRequests[0]["Identifier"] != "db-ORDERS" ||
		f.piRequests[0]["ServiceType"] != "RDS" {
		t.Fatalf("PI request = %v", f.piRequests)
	}
}

// assertSigned checks every request carried SigV4 with the access key and
// never the secret or session token in the Authorization header.
func assertSigned(t *testing.T, f *fakeAWS) {
	t.Helper()
	if len(f.authHeader) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, h := range f.authHeader {
		if !strings.HasPrefix(h, "AWS4-HMAC-SHA256 Credential="+testAccessKey+"/") {
			t.Fatalf("unsigned request: %q", h)
		}
		if strings.Contains(h, testSecretKey) {
			t.Fatal("secret key leaked into Authorization header")
		}
	}
}

func TestAWSCollectWithoutPerformanceInsightsUsesInstanceClass(t *testing.T) {
	f := rdsFixture(t, false)
	s := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"}))
	if f.called("pi") != 0 {
		t.Fatalf("PI called %d times although it is disabled", f.called("pi"))
	}
	wantPoint(t, "memory total", s.MemoryTotalBytes, 16*gib)
	if s.MemoryTotalSource != MemorySourceInstanceClass || s.DBLoad != nil {
		t.Fatalf("source=%q load=%v", s.MemoryTotalSource, s.DBLoad)
	}
	if !containsPrefix(s.Missing, "db_load") {
		t.Fatalf("missing = %v, want db_load reported", s.Missing)
	}
}

func TestAWSCollectAuroraClusterEndpointResolvesWriter(t *testing.T) {
	f := newFakeAWS(t, awsNow)
	f.clusters["shop"] = clusterXML("shop", "shop-1", "shop-2")
	f.instances["shop-1"] = instanceXML("shop-1", "db.r7g.xlarge", "aurora-postgresql",
		"default.aurora-postgresql16", "in-sync", 1, 0, false, "shop")
	f.metric("shop-1", "CPUUtilization", time.Minute, 12)
	f.metric("shop-1", "FreeableMemory", time.Minute, 20*gib)
	f.metric("shop-1", "AuroraReplicaLagMaximum", time.Minute, 1500) // milliseconds
	s := mustCollect(t, f.source(t, AWSOptions{ClusterID: "shop"}))
	if s.Provider != "aurora" || s.Resource != "aurora:us-east-1/shop-1" {
		t.Fatalf("provider/resource = %q/%q", s.Provider, s.Resource)
	}
	wantPoint(t, "cpu", s.CPUPct, 12)
	wantPoint(t, "aurora lag seconds", s.ReplicaLagSeconds, 1.5)
	wantPoint(t, "memory (r7g.xlarge)", s.MemoryTotalBytes, 32*gib)
	if !s.StorageAutoGrows || s.FreeStorageBytes != nil || s.StorageCapacityBytes != nil {
		t.Fatalf("aurora storage must be auto-growing and unbounded: %+v", s)
	}
	for _, q := range f.cwQueries {
		if strings.Contains(q.Encode(), "FreeStorageSpace") {
			t.Fatal("aurora must not query FreeStorageSpace")
		}
	}
}

func TestAWSCollectAuroraReaderEndpointIsUnavailable(t *testing.T) {
	_, err := NewAWSSource(AWSOptions{Region: "us-east-1", ClusterID: "shop",
		ReaderEndpoint: true, Credentials: staticCreds()})
	if !errors.Is(err, ErrIdentity) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reader endpoint err = %v, want ErrIdentity", err)
	}
}

func TestNewAWSSourceRejectsInvalidIdentity(t *testing.T) {
	cases := []AWSOptions{
		{Region: "us-east-1"},
		{Region: "us-east-1", InstanceID: "a", ClusterID: "b"},
		{Region: "us-east-1", InstanceID: "bad;id"},
		{Region: "nowhere", InstanceID: "orders"},
		{Region: "", InstanceID: "orders"},
	}
	for _, opts := range cases {
		opts.Credentials = staticCreds()
		if _, err := NewAWSSource(opts); !errors.Is(err, ErrIdentity) {
			t.Errorf("NewAWSSource(%+v) err = %v, want ErrIdentity", opts, err)
		}
	}
}

func TestAWSCollectErrorsAreDistinguishable(t *testing.T) {
	cases := []struct {
		name string
		key  string
		fail fakeFailure
		want error
	}{
		{"cloudwatch throttled", "cw", fakeFailure{status: 400, code: "Throttling",
			message: "Rate exceeded"}, ErrThrottled},
		{"cloudwatch access denied", "cw", fakeFailure{status: 403, code: "AccessDenied",
			message: "not authorized to perform cloudwatch:GetMetricData"}, ErrAuth},
		{"cloudwatch expired token", "cw", fakeFailure{status: 403, code: "ExpiredToken",
			message: "expired"}, ErrAuth},
		{"cloudwatch skewed", "cw", fakeFailure{status: 400, code: "RequestTimeTooSkewed",
			message: "too skewed"}, ErrClockSkew},
		{"cloudwatch signature expired", "cw", fakeFailure{status: 403,
			code: "SignatureDoesNotMatch", message: "Signature expired: 20261004T110000Z is " +
				"now earlier than 20261004T115500Z"}, ErrClockSkew},
		{"cloudwatch malformed", "cw", fakeFailure{status: 200, body: "<not-xml"},
			ErrMalformed},
		{"cloudwatch 500", "cw", fakeFailure{status: 500, code: "InternalFailure",
			message: "boom"}, ErrProvider},
		{"rds invalid token", "rds:DescribeDBInstances", fakeFailure{status: 403,
			code: "InvalidClientTokenId", message: "bad"}, ErrAuth},
		{"rds throttled", "rds:DescribeDBInstances", fakeFailure{status: 400,
			code: "Throttling", message: "slow down"}, ErrThrottled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := rdsFixture(t, true)
			f.fail[tc.key] = tc.fail
			_, err := f.source(t, AWSOptions{InstanceID: "orders"}).Collect(
				context.Background(), awsNow)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			assertNoSecrets(t, err)
		})
	}
}

func assertNoSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range []string{testSecretKey, testToken} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error leaks a credential: %v", err)
		}
	}
}

func TestAWSCollectInstanceNotFoundIsIdentityError(t *testing.T) {
	f := rdsFixture(t, true)
	_, err := f.source(t, AWSOptions{InstanceID: "missing"}).Collect(
		context.Background(), awsNow)
	if !errors.Is(err, ErrIdentity) {
		t.Fatalf("err = %v, want ErrIdentity", err)
	}
}

// Performance Insights is optional evidence: its failures are partial
// data, never a failed collection.
func TestAWSCollectPerformanceInsightsFailureIsPartial(t *testing.T) {
	for _, code := range []string{"NotAuthorizedException", "ThrottlingException"} {
		f := rdsFixture(t, true)
		f.fail["pi"] = fakeFailure{status: 400, code: code, message: "x"}
		s := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"}))
		if s.DBLoad != nil || s.MemoryTotalSource != MemorySourceInstanceClass {
			t.Fatalf("%s: load=%v memsource=%q", code, s.DBLoad, s.MemoryTotalSource)
		}
		wantPoint(t, code+" cpu still collected", s.CPUPct, 37.5)
		if !containsPrefix(s.Missing, "db_load") {
			t.Fatalf("%s: missing = %v", code, s.Missing)
		}
	}
}

func TestAWSCollectPartialMetrics(t *testing.T) {
	f := newFakeAWS(t, awsNow)
	f.instances["orders"] = instanceXML("orders", "db.unknown.huge", "postgres",
		"orders-pg16", "in-sync", 100, 0, false, "", "orders-r1")
	f.metric("orders", "CPUUtilization", time.Minute, 50)
	f.metric("orders", "FreeableMemory", time.Minute, -5)        // invalid: refused
	f.metric("orders", "DatabaseConnections", 40*time.Minute, 9) // outside the window
	s := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"}))
	wantPoint(t, "cpu", s.CPUPct, 50)
	if s.FreeableMemoryBytes != nil || s.FreeStorageBytes != nil ||
		s.ReplicaLagSeconds != nil || s.MemoryTotalBytes != nil {
		t.Fatalf("absent or invalid metrics must stay nil: %+v", s)
	}
	for _, want := range []string{"freeable_memory", "free_storage", "replica_lag",
		"memory_total", "connections"} {
		if !containsPrefix(s.Missing, want) {
			t.Fatalf("missing = %v, want %s", s.Missing, want)
		}
	}
	wantPoint(t, "capacity without autoscaling", s.StorageCapacityBytes, 100*gib)
}

func TestAWSCollectRejectsClockSkew(t *testing.T) {
	t.Run("server date", func(t *testing.T) {
		f := rdsFixture(t, true)
		f.dateSkew = time.Hour
		_, err := f.source(t, AWSOptions{InstanceID: "orders"}).Collect(
			context.Background(), awsNow)
		if !errors.Is(err, ErrClockSkew) {
			t.Fatalf("err = %v, want ErrClockSkew", err)
		}
	})
	t.Run("future datapoint", func(t *testing.T) {
		f := rdsFixture(t, true)
		f.metric("orders", "CPUUtilization", -20*time.Minute, 10) // 20 min ahead
		_, err := f.source(t, AWSOptions{InstanceID: "orders"}).Collect(
			context.Background(), awsNow)
		if !errors.Is(err, ErrClockSkew) {
			t.Fatalf("err = %v, want ErrClockSkew", err)
		}
	})
	t.Run("small skew is tolerated", func(t *testing.T) {
		f := rdsFixture(t, true)
		f.dateSkew = 90 * time.Second
		mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"}))
	})
}

func TestAWSCollectWithoutCredentialsIsUnavailable(t *testing.T) {
	f := rdsFixture(t, true)
	src := f.source(t, AWSOptions{InstanceID: "orders", Credentials: failingCreds()})
	_, err := src.Collect(context.Background(), awsNow)
	if !errors.Is(err, ErrNoCredentials) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	if n := len(f.authHeader); n != 0 {
		t.Fatalf("%d requests sent without credentials", n)
	}
}

func TestAWSCollectHonoursCancelledContext(t *testing.T) {
	f := rdsFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.source(t, AWSOptions{InstanceID: "orders"}).Collect(ctx, awsNow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
