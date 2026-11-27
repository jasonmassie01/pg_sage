package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/config"
)

const testAzureSub = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"

// CHECK-02: Azure gets a polling telemetry runtime; telemetry no longer
// needs a managed-change resolver (Azure's comes from the ARM adapter).
func TestCloudTelemetryForAzureReturnsAPollingRuntime(t *testing.T) {
	c := cloudCfg()
	c.Azure.SubscriptionID, c.Azure.ResourceGroup = testAzureSub, "rg-shop"
	rt, resolver := cloudTelemetryFor(context.Background(), c, "azure",
		"orders.postgres.database.azure.com", "app", true, config.CloudResourceConfig{},
		testCloudDeps())
	if rt == nil {
		t.Fatal("azure runtime = nil")
	}
	if !rt.HasSource() || resolver != nil {
		t.Fatalf("source=%t resolver=%v, want a polling runtime without a resolver",
			rt.HasSource(), resolver)
	}
	if st := rt.Status(); st.Provider != "azure" ||
		st.Reason != "unavailable: waiting for the first sample" {
		t.Fatalf("status = %+v", st)
	}
}

func TestCloudTelemetryForAzurePerDatabaseResourceWins(t *testing.T) {
	c := cloudCfg() // no process azure.* settings
	r := config.CloudResourceConfig{Azure: config.CloudResourceAzure{
		Subscription: testAzureSub, ResourceGroup: "rg-fleet", Server: "member-2"}}
	rt, _ := cloudTelemetryFor(context.Background(), c, "azure", "10.1.2.3", "app", false,
		r, testCloudDeps())
	if rt == nil || !rt.HasSource() {
		t.Fatalf("fleet member with cloud_resource.azure: %+v", rt)
	}
	rt, _ = cloudTelemetryFor(context.Background(), c, "azure", "10.1.2.3", "app", false,
		config.CloudResourceConfig{}, testCloudDeps())
	if rt == nil || rt.HasSource() || !strings.Contains(rt.Status().Reason, "azure.server_name") &&
		!strings.Contains(rt.Status().Reason, "azure.subscription_id") {
		t.Fatalf("unnamed azure member: %+v", rt.Status())
	}
}

func TestCloudTelemetryForAzureCredentialFailure(t *testing.T) {
	c := cloudCfg()
	c.Azure.SubscriptionID, c.Azure.ResourceGroup = testAzureSub, "rg-shop"
	deps := testCloudDeps()
	deps.azureToken = func() (azure.TokenSource, error) {
		return nil, errors.New("azure credential: no identity configured")
	}
	rt, resolver := cloudTelemetryFor(context.Background(), c, "azure",
		"orders.postgres.database.azure.com", "app", true, config.CloudResourceConfig{}, deps)
	if rt == nil || rt.HasSource() || resolver != nil ||
		!strings.Contains(rt.Status().Reason, "no identity configured") {
		t.Fatalf("credential failure: %+v", rt.Status())
	}
}

func TestCloudTelemetryForAlloyDBFleetResource(t *testing.T) {
	r := config.CloudResourceConfig{GCP: config.CloudResourceGCP{Project: "shop-prod",
		Cluster: "orders", Instance: "orders-primary"}}
	rt, resolver := cloudTelemetryFor(context.Background(), cloudCfg(), "alloydb",
		"10.30.0.5", "app", false, r, testCloudDeps())
	if rt == nil || !rt.HasSource() || resolver != nil {
		t.Fatalf("alloydb member: rt=%+v resolver=%v", rt, resolver)
	}
	if st := rt.Status(); st.Provider != "alloydb" {
		t.Fatalf("status = %+v", st)
	}
	deps := testCloudDeps()
	deps.gcpToken = func(context.Context) (gcpTokenSource, error) { return fakeGCPToken{}, nil }
	r.GCP.Project = ""
	rt, _ = cloudTelemetryFor(context.Background(), cloudCfg(), "alloydb", "10.30.0.5",
		"app", false, r, deps)
	if rt == nil || rt.HasSource() || !strings.Contains(rt.Status().Reason, "project") {
		t.Fatalf("alloydb without a project: %+v", rt.Status())
	}
}

func TestCloudShareKeyNamesAzureAndAlloyDBResources(t *testing.T) {
	ct := config.CloudTelemetryConfig{}
	az := config.CloudResourceConfig{Azure: config.CloudResourceAzure{
		Subscription: testAzureSub, ResourceGroup: "rg", Server: "one"}}
	a := cloudShareKey("azure", "10.0.0.1", 5432, az, ct)
	b := cloudShareKey("azure", "10.0.0.2", 5432, az, ct)
	if a != b || !strings.Contains(a, "azure="+testAzureSub+"/rg/one") {
		t.Fatalf("azure keys %q %q: two hosts of one named server must share", a, b)
	}
	host := cloudShareKey("azure", "one.postgres.database.azure.com", 5432, az, ct)
	if host == a {
		t.Fatalf("a host naming the server wins over the configured resource: %q", host)
	}
	al := config.CloudResourceConfig{GCP: config.CloudResourceGCP{Project: "shop-prod",
		Cluster: "orders", Instance: "p"}}
	k := cloudShareKey("alloydb", "10.0.0.1", 5432, al, ct)
	if !strings.Contains(k, "alloydb=shop-prod/orders/p") {
		t.Fatalf("alloydb key = %q", k)
	}
}
