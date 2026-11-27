package clone_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
)

// lab-05g CHECK-41 (OWNER ACTION, decision D2): the clone contract on a
// real, funded Azure Database for PostgreSQL flexible server. Each check
// restores a whole billable server holding production data, so it runs
// only with SAGE_AZURE_LIVE=1 and
//
//	SAGE_AZURE_LIVE_DATABASE_URL    the monitored server's DSN on its FQDN
//	SAGE_AZURE_SUBSCRIPTION_ID      its subscription
//	SAGE_AZURE_RESOURCE_GROUP       its resource group
//	SAGE_AZURE_SERVER               its server name
//	SAGE_TEST_DATABASE_URL          a scratch database for the receipts
//
// with Azure credentials (AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET,
// a managed identity, or az login). Afterwards no sage-* server may be
// listed by az postgres flexible-server list --resource-group <group>.
const azLiveFlag = "SAGE_AZURE_LIVE"

func TestAzureRestoreLive_ContractOnARealServer(t *testing.T) {
	contracttest.RequireLive(t, azLiveFlag)
	dsn := os.Getenv("SAGE_AZURE_LIVE_DATABASE_URL")
	sub, group := os.Getenv("SAGE_AZURE_SUBSCRIPTION_ID"), os.Getenv("SAGE_AZURE_RESOURCE_GROUP")
	server := os.Getenv("SAGE_AZURE_SERVER")
	if dsn == "" || sub == "" || group == "" || server == "" {
		t.Fatal(azLiveFlag + " is set but SAGE_AZURE_LIVE_DATABASE_URL, " +
			"SAGE_AZURE_SUBSCRIPTION_ID, SAGE_AZURE_RESOURCE_GROUP or SAGE_AZURE_SERVER is not")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prodPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid SAGE_AZURE_LIVE_DATABASE_URL")
	}
	t.Cleanup(prodPool.Close)
	identity, err := clone.PoolIdentity(prodPool)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := azure.DefaultTokenSource()
	if err != nil {
		t.Fatal(err)
	}
	pool := azReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		p, err := clone.NewAzureRestoreProvider(clone.AzureRestoreConfig{
			SubscriptionID: sub, ResourceGroup: group, Server: server,
			Deadline: 2 * time.Hour, TTL: 4 * time.Hour, Token: token,
			Production: func(context.Context) (clone.AzureProduction, error) {
				return clone.AzureProduction{DSN: dsn}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return contracttest.Subject{Adapter: clone.AzureRestoreAdapter, Provider: p,
			Production: identity, Exists: func(ctx context.Context, ref string) (bool, error) {
				_, found, err := p.FindByName(ctx, ref)
				return found, err
			}}
	}, contracttest.Options{Pool: pool, DeploymentID: azDeployment})
}
