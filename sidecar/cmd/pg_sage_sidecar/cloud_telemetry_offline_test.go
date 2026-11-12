package main

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// errOfflineTest is every credential chain's answer in runtime builds.
var errOfflineTest = errors.New("offline test: no provider credentials")

// stubCloudTelemetryOffline keeps runtime builds (fleet, parity) from
// reaching a provider: every credential chain fails, so a managed database
// gets an unavailable telemetry runtime and no request leaves the test.
func stubCloudTelemetryOffline(t *testing.T) {
	t.Helper()
	orig := newCloudTelemetry
	t.Cleanup(func() { newCloudTelemetry = orig })
	deps := cloudDeps{
		awsCredentials: func(context.Context, string) (aws.CredentialsProvider, error) {
			return nil, errOfflineTest
		},
		gcpToken:   func(context.Context) (gcpTokenSource, error) { return nil, errOfflineTest },
		azureToken: func() (azure.TokenSource, error) { return nil, errOfflineTest },
	}
	newCloudTelemetry = func(ctx context.Context, rt *databaseRuntime) (*cloudtel.Runtime,
		managedparam.Resolver) {
		return runtimeCloudTelemetry(ctx, rt, deps)
	}
}
