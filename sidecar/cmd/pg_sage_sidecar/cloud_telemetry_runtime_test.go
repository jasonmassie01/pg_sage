package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

type fakeGCPToken struct{ project string }

func (f fakeGCPToken) Token(context.Context) (string, error) { return "ya29.x", nil }
func (f fakeGCPToken) Project() string                       { return f.project }

func testCloudDeps() cloudDeps {
	return cloudDeps{
		awsCredentials: func(context.Context, string) (aws.CredentialsProvider, error) {
			return aws.AnonymousCredentials{}, nil
		},
		gcpToken: func(context.Context) (gcpTokenSource, error) {
			return fakeGCPToken{project: "proj-adc"}, nil
		},
	}
}

func cloudCfg() *config.Config {
	c := config.DefaultConfig()
	return c
}

func TestCloudTelemetryForProviders(t *testing.T) {
	ctx := context.Background()
	deps := testCloudDeps()
	cases := []struct {
		name, provider, host string
		standalone           bool
		mutate               func(*config.Config)
		wantNil              bool
		wantProvider         string
		wantAvailableReason  string // "" = a polling runtime
	}{
		{name: "self-managed has none", provider: "self-managed", host: "db", wantNil: true},
		{name: "azure has none", provider: "azure", host: "x.postgres.database.azure.com",
			wantNil: true},
		{name: "disabled", provider: "rds",
			host:    "orders.c9akciq32.us-east-1.rds.amazonaws.com",
			mutate:  func(c *config.Config) { f := false; c.CloudTelemetry.Enabled = &f },
			wantNil: true},
		{name: "rds from host", provider: "rds",
			host: "orders.c9akciq32.us-east-1.rds.amazonaws.com", wantProvider: "rds"},
		{name: "aurora cluster endpoint", provider: "aurora",
			host: "shop.cluster-c9akciq32.us-west-2.rds.amazonaws.com", wantProvider: "aurora"},
		{name: "aurora reader endpoint", provider: "aurora",
			host:         "shop.cluster-ro-c9akciq32.us-west-2.rds.amazonaws.com",
			wantProvider: "aurora", wantAvailableReason: "reader endpoint"},
		{name: "rds behind an IP needs a name", provider: "rds", host: "10.0.0.5",
			standalone: true, wantProvider: "rds",
			wantAvailableReason: "cloud_telemetry.aws.db_instance_identifier"},
		{name: "rds IP with configured name", provider: "rds", host: "10.0.0.5",
			standalone: true, wantProvider: "rds", mutate: func(c *config.Config) {
				c.CloudTelemetry.AWS.Region = "us-east-1"
				c.CloudTelemetry.AWS.DBInstanceIdentifier = "orders"
			}},
		{name: "fleet ignores configured names", provider: "rds", host: "10.0.0.5",
			wantProvider: "rds", mutate: func(c *config.Config) {
				c.CloudTelemetry.AWS.Region = "us-east-1"
				c.CloudTelemetry.AWS.DBInstanceIdentifier = "orders"
			}, wantAvailableReason: "host"},
		{name: "cloud sql by IP and ADC project", provider: "cloud-sql", host: "10.20.0.5",
			wantProvider: "cloud-sql"},
		{name: "cloud sql by hostname cannot be matched", provider: "cloud-sql",
			host: "db.internal.example", wantProvider: "cloud-sql",
			wantAvailableReason: "cloud_telemetry.gcp.instance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cloudCfg()
			if tc.mutate != nil {
				tc.mutate(c)
			}
			rt, _ := cloudTelemetryFor(ctx, c, tc.provider, tc.host, "orders",
				tc.standalone, deps)
			if tc.wantNil {
				if rt != nil {
					t.Fatalf("runtime = %+v, want nil", rt.Status())
				}
				return
			}
			if rt == nil {
				t.Fatal("runtime = nil")
			}
			st := rt.Status()
			if st.Provider != tc.wantProvider || st.Database != "orders" {
				t.Fatalf("status = %+v", st)
			}
			if tc.wantAvailableReason == "" {
				if strings.HasPrefix(st.Reason, "unavailable: ") &&
					!strings.Contains(st.Reason, "first sample") {
					t.Fatalf("want a polling runtime, got %q", st.Reason)
				}
				return
			}
			if st.Available || !strings.Contains(st.Reason, tc.wantAvailableReason) {
				t.Fatalf("reason = %q, want it to mention %q", st.Reason,
					tc.wantAvailableReason)
			}
		})
	}
}

func TestCloudTelemetryForCredentialFailures(t *testing.T) {
	ctx := context.Background()
	deps := testCloudDeps()
	deps.gcpToken = func(context.Context) (gcpTokenSource, error) {
		return nil, cloudtel.ErrNoCredentials
	}
	rt, resolver := cloudTelemetryFor(ctx, cloudCfg(), "cloud-sql", "10.20.0.5", "app",
		true, deps)
	if rt == nil || rt.Status().Available || resolver != nil ||
		!strings.Contains(rt.Status().Reason, "credentials") {
		t.Fatalf("no GCP credentials: %+v resolver=%v", rt.Status(), resolver)
	}
	deps = testCloudDeps()
	deps.awsCredentials = func(context.Context, string) (aws.CredentialsProvider, error) {
		return nil, errors.New("shared config profile prod not found")
	}
	rt, _ = cloudTelemetryFor(ctx, cloudCfg(), "rds",
		"orders.c9akciq32.us-east-1.rds.amazonaws.com", "app", true, deps)
	if rt == nil || rt.Status().Available ||
		!strings.Contains(rt.Status().Reason, "profile prod not found") {
		t.Fatalf("AWS config failure: %+v", rt.Status())
	}
	deps = testCloudDeps()
	deps.gcpToken = func(context.Context) (gcpTokenSource, error) {
		return fakeGCPToken{}, nil // ADC without a project
	}
	rt, _ = cloudTelemetryFor(ctx, cloudCfg(), "cloud-sql", "10.20.0.5", "app", true, deps)
	if rt == nil || !strings.Contains(rt.Status().Reason, "cloud_telemetry.gcp.project") {
		t.Fatalf("no project: %+v", rt.Status())
	}
}
