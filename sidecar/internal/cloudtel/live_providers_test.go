//go:build cloudlive

package cloudtel

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// Owner-run live roots for Azure flexible server and AlloyDB telemetry
// (build tag cloudlive). They only read ARM metadata and metrics; nothing
// is modified and no resource is created. Without their variables they
// skip with the reason, since no disposable resource may exist yet.

func TestLiveAzureTelemetry(t *testing.T) {
	sub, group := os.Getenv("SAGE_AZURE_SUBSCRIPTION_ID"), os.Getenv("SAGE_AZURE_RESOURCE_GROUP")
	server := os.Getenv("SAGE_AZURE_SERVER_NAME")
	if sub == "" || group == "" || server == "" {
		t.Skip("no disposable flexible server: set SAGE_AZURE_SUBSCRIPTION_ID, " +
			"SAGE_AZURE_RESOURCE_GROUP and SAGE_AZURE_SERVER_NAME " +
			"(scripts/azure/provision-test-server.sh writes them) and log in with az")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	token := func(ctx context.Context) (string, error) {
		tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: []string{"https://management.azure.com/.default"}})
		return tok.Token, err
	}
	src, err := NewAzureSource(AzureOptions{SubscriptionID: sub, ResourceGroup: group,
		Server: server, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := src.Collect(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUPct == nil || s.MemoryTotalBytes == nil || s.FreeableMemoryBytes == nil {
		t.Fatalf("live sample lacks fresh CPU or memory: %+v missing=%v", s, s.Missing)
	}
	t.Logf("CHECK-CLOUD-05: PASS %s cpu=%.1f%% mem=%.0f avail=%.0f missing=%v", s.Resource,
		s.CPUPct.Value, s.MemoryTotalBytes.Value, s.FreeableMemoryBytes.Value, s.Missing)
}

func TestLiveAlloyDBTelemetry(t *testing.T) {
	project, cluster := os.Getenv("SAGE_TEST_GCP_PROJECT"), os.Getenv("SAGE_TEST_ALLOYDB_CLUSTER")
	instance := os.Getenv("SAGE_TEST_ALLOYDB_INSTANCE")
	if project == "" || cluster == "" || instance == "" {
		t.Skip("no AlloyDB instance funded yet (owner question 6): set " +
			"SAGE_TEST_GCP_PROJECT, SAGE_TEST_ALLOYDB_CLUSTER and SAGE_TEST_ALLOYDB_INSTANCE " +
			"(and ADC with roles/monitoring.viewer)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	token, err := NewGCPTokenSource(ctx, GCPAuthOptions{
		CredentialsFile: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")})
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewAlloyDBSource(AlloyDBOptions{Project: project, Cluster: cluster,
		Instance: instance, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Collect(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUPct == nil || s.FreeableMemoryBytes == nil {
		t.Fatalf("live sample lacks fresh CPU or memory: %+v missing=%v", s, s.Missing)
	}
	t.Logf("CHECK-CLOUD-06: PASS %s cpu=%.1f%% avail=%.0f missing=%v", s.Resource,
		s.CPUPct.Value, s.FreeableMemoryBytes.Value, s.Missing)
}
