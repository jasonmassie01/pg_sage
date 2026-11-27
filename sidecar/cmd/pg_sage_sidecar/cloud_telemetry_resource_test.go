package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests: telemetry identity is resolved once per
// runtime build from immutable inputs.

func gcpResource(project, instance string) config.CloudResourceConfig {
	return config.CloudResourceConfig{GCP: config.CloudResourceGCP{
		Project: project, Instance: instance}}
}

// Boundary: a Cloud SQL Auth Proxy unix socket names the project and the
// instance, so the instance is read directly and never listed.
func TestCloudSQLSocketHostNamesTheInstance(t *testing.T) {
	opts, err := gcpOptionsFor(config.CloudTelemetryConfig{}, config.CloudResourceGCP{},
		"/cloudsql/proj-1:us-central1:main", false)
	if err != nil {
		t.Fatalf("gcpOptionsFor: %v", err)
	}
	if opts.Project != "proj-1" || opts.Instance != "main" || opts.HostIP != "" {
		t.Fatalf("options = %+v, want Project=proj-1 Instance=main and no HostIP", opts)
	}
}

func TestCloudSQLSocketHostRejectsMalformedPaths(t *testing.T) {
	for _, host := range []string{
		"/cloudsql/proj-1:main", "/cloudsql/", "/cloudsql/proj-1:us-central1:",
		"/cloudsql/proj-1:us-central1:main:extra", "/var/run/postgresql",
		"/cloudsql/proj-1:us-central1:Main_DB",
	} {
		if _, err := gcpOptionsFor(config.CloudTelemetryConfig{}, config.CloudResourceGCP{},
			host, false); err == nil || !strings.Contains(err.Error(),
			"cloud_resource.gcp.instance") {
			t.Errorf("host %q: error = %v, want a refusal naming cloud_resource.gcp.instance",
				host, err)
		}
	}
}

// sqlAdminFake counts Cloud SQL Admin calls by kind.
type sqlAdminFake struct {
	mu          sync.Mutex
	lists, gets int
}

func (f *sqlAdminFake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/instances"):
		f.lists++
	case strings.Contains(r.URL.Path, "/instances/"):
		f.gets++
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.URL.Path, "/instances/main") {
		_, _ = w.Write([]byte(`{"name":"main","databaseVersion":"POSTGRES_16",` +
			`"settings":{"tier":"db-custom-2-7680"}}`))
		return
	}
	_, _ = w.Write([]byte(`{}`))
}

// Boundary: the socket host gives zero instances.list calls.
func TestCloudSQLSocketHostMakesNoInstanceListCall(t *testing.T) {
	fake := &sqlAdminFake{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	opts, err := gcpOptionsFor(config.CloudTelemetryConfig{}, config.CloudResourceGCP{},
		"/cloudsql/proj-1:us-central1:main", false)
	if err != nil {
		t.Fatalf("gcpOptionsFor: %v", err)
	}
	opts.Token, opts.HTTPClient = fakeGCPToken{project: "proj-adc"}, srv.Client()
	opts.SQLAdminEndpoint, opts.MonitoringEndpoint = srv.URL, srv.URL
	src, err := cloudtel.NewGCPSource(opts)
	if err != nil {
		t.Fatalf("NewGCPSource: %v", err)
	}
	_, _ = src.Collect(context.Background(), time.Now()) // monitoring is a stub
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.lists != 0 || fake.gets == 0 {
		t.Fatalf("instances.list = %d, instances.get = %d; want 0 and at least 1",
			fake.lists, fake.gets)
	}
}

// countingDeps records whether any credential chain was consulted.
func countingDeps(calls *int) cloudDeps {
	deps := testCloudDeps()
	gcp, awsCreds := deps.gcpToken, deps.awsCredentials
	deps.gcpToken = func(ctx context.Context) (gcpTokenSource, error) {
		*calls++
		return gcp(ctx)
	}
	deps.awsCredentials = func(ctx context.Context, region string) (aws.CredentialsProvider,
		error) {
		*calls++
		return awsCreds(ctx, region)
	}
	return deps
}

// Boundary: the Auth Proxy on loopback with no named resource is
// unavailable, says why, and never reaches Google.
func TestCloudSQLLoopbackWithoutResourceNamesTheAuthProxy(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost", "127.0.0.2"} {
		calls := 0
		rt, _ := cloudTelemetryFor(context.Background(), cloudCfg(), "cloud-sql", host,
			"orders", false, config.CloudResourceConfig{}, countingDeps(&calls))
		if rt == nil {
			t.Fatalf("%s: runtime = nil", host)
		}
		st := rt.Status()
		if st.Available || !strings.Contains(st.Reason, "Auth Proxy") ||
			!strings.Contains(st.Reason, "cloud_resource.gcp.instance") {
			t.Fatalf("%s: status = %+v, want unavailable naming the Auth Proxy", host, st)
		}
		if calls != 0 {
			t.Fatalf("%s: credential calls = %d, want 0", host, calls)
		}
	}
}

func assertPolling(t *testing.T, rt *cloudtel.Runtime, provider string) {
	t.Helper()
	if rt == nil {
		t.Fatal("runtime = nil")
	}
	st := rt.Status()
	if st.Provider != provider || st.Database != "orders" {
		t.Fatalf("status = %+v, want provider %s", st, provider)
	}
	if strings.HasPrefix(st.Reason, "unavailable: ") && !strings.Contains(st.Reason,
		"first sample") {
		t.Fatalf("want a polling runtime, got %q", st.Reason)
	}
}

// A named instance on loopback (fleet mode) polls.
func TestCloudSQLLoopbackWithNamedInstancePolls(t *testing.T) {
	rt, _ := cloudTelemetryFor(context.Background(), cloudCfg(), "cloud-sql", "127.0.0.1",
		"orders", false, gcpResource("proj-1", "main"), testCloudDeps())
	assertPolling(t, rt, "cloud-sql")
}

// Fleet: databases[].provider rds plus cloud_resource.aws resolves
// telemetry behind an IP, with the configured region.
func TestFleetRDSResourceResolvesTelemetryBehindAnIP(t *testing.T) {
	var regions []string
	deps := testCloudDeps()
	deps.awsCredentials = func(_ context.Context, region string) (aws.CredentialsProvider,
		error) {
		regions = append(regions, region)
		return aws.AnonymousCredentials{}, nil
	}
	resource := config.CloudResourceConfig{AWS: config.CloudResourceAWS{
		DBInstanceIdentifier: "orders-prod", Region: "eu-west-1"}}
	rt, _ := cloudTelemetryFor(context.Background(), cloudCfg(), "rds", "10.0.0.5",
		"orders", false, resource, deps)
	assertPolling(t, rt, "rds")
	if len(regions) != 1 || regions[0] != "eu-west-1" {
		t.Fatalf("credential regions = %v, want [eu-west-1]", regions)
	}
}

// An Aurora cluster named for a fleet database behind custom DNS polls.
func TestFleetAuroraClusterResourcePolls(t *testing.T) {
	resource := config.CloudResourceConfig{AWS: config.CloudResourceAWS{
		ClusterIdentifier: "shop", Region: "us-west-2"}}
	rt, _ := cloudTelemetryFor(context.Background(), cloudCfg(), "aurora",
		"db.shop.example.com", "orders", false, resource, testCloudDeps())
	assertPolling(t, rt, "aurora")
}

// Without a resource a fleet RDS behind an IP stays unavailable and names
// the per-database key.
func TestFleetRDSBehindAnIPWithoutResourceNamesTheKey(t *testing.T) {
	rt, _ := cloudTelemetryFor(context.Background(), cloudCfg(), "rds", "10.0.0.5",
		"orders", false, config.CloudResourceConfig{}, testCloudDeps())
	if rt == nil {
		t.Fatal("runtime = nil")
	}
	st := rt.Status()
	if st.Available || !strings.Contains(st.Reason,
		"cloud_resource.aws.db_instance_identifier") {
		t.Fatalf("status = %+v, want unavailable naming cloud_resource", st)
	}
}

// A named resource never overrides what the host itself names.
func TestRDSHostWinsOverNamedResource(t *testing.T) {
	var regions []string
	deps := testCloudDeps()
	deps.awsCredentials = func(_ context.Context, region string) (aws.CredentialsProvider,
		error) {
		regions = append(regions, region)
		return aws.AnonymousCredentials{}, nil
	}
	resource := config.CloudResourceConfig{AWS: config.CloudResourceAWS{
		DBInstanceIdentifier: "other", Region: "eu-west-1"}}
	rt, _ := cloudTelemetryFor(context.Background(), cloudCfg(), "rds",
		"orders.c9akciq32.us-east-1.rds.amazonaws.com", "orders", false, resource, deps)
	assertPolling(t, rt, "rds")
	if len(regions) != 1 || regions[0] != "us-east-1" {
		t.Fatalf("credential regions = %v, want the host's us-east-1", regions)
	}
}

func TestAzureTargetConfigPrefersTheDatabaseResource(t *testing.T) {
	process := azureCfg("0b1f6471-1bf0-4dda-aec3-111122223333", "rg-proc", "srv-proc")
	if got := azureTargetConfig(process, config.CloudResourceAzure{}); got != process {
		t.Fatal("without a resource the process config must be used as is")
	}
	r := config.CloudResourceAzure{Subscription: "1b1f6471-1bf0-4dda-aec3-111122223333",
		ResourceGroup: "rg-db", Server: "flex-db"}
	got := azureTargetConfig(process, r)
	if got.Azure.SubscriptionID != r.Subscription || got.Azure.ResourceGroup != "rg-db" ||
		got.Azure.ServerName != "flex-db" {
		t.Fatalf("azure target = %+v, want the database resource", got.Azure)
	}
	if process.Azure.ServerName != "srv-proc" {
		t.Fatal("the process config was modified")
	}
	if azureTargetConfig(nil, r).Azure.ServerName != "flex-db" {
		t.Fatal("a nil process config must still carry the database resource")
	}
}
