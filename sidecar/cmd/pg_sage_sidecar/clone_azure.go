package main

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/azure"
	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
)

// azureRestoreProvider is the azure_restore adapter (lab-05g): one
// point-in-time restore of the monitored Azure flexible server per
// rehearsal, a full copy, through the DefaultAzureCredential chain
// (newAzureTokenSource, the one server parameters use). monitored is the
// production database; without it the adapter can destroy but never
// creates. A clone lives at most the rehearsal receipt's TTL, which is
// tagged on it.
func azureRestoreProvider(cloneCfg config.CloneProviderConfig,
	monitored *pgxpool.Pool) (clonepkg.Provider, error) {
	s := cloneCfg.Azure
	p, err := clonepkg.NewAzureRestoreProvider(clonepkg.AzureRestoreConfig{
		SubscriptionID: s.SubscriptionID, ResourceGroup: s.ResourceGroup, Server: s.Server,
		Deadline: restoreDeadline(cloneCfg), TTL: rehearsalReceiptTTL(cloneCfg),
		Token: azureCloneToken(), Production: azureProduction(monitored),
	})
	if err != nil {
		// A nil interface, not a typed nil: callers test the adapter for nil.
		return nil, err
	}
	return p, nil
}

// azureCloneToken is an ARM token from the credential chain, built on
// first use (never at startup, and never for a create the adapter
// refuses) and reused: the chain caches and refreshes its tokens. A chain
// that cannot be built fails every call with its error.
func azureCloneToken() func(context.Context) (string, error) {
	var once sync.Once
	var source azure.TokenSource
	var buildErr error
	return func(ctx context.Context) (string, error) {
		once.Do(func() { source, buildErr = newAzureTokenSource() })
		if buildErr != nil {
			return "", buildErr
		}
		return source(ctx)
	}
}

// azureProduction is the DSN pool was opened with: the monitored server's
// FQDN and the users a clone reuses. nil for a nil pool.
func azureProduction(
	pool *pgxpool.Pool,
) func(context.Context) (clonepkg.AzureProduction, error) {
	if pool == nil {
		return nil
	}
	return func(context.Context) (clonepkg.AzureProduction, error) {
		return clonepkg.AzureProduction{DSN: pool.Config().ConnString()}, nil
	}
}
