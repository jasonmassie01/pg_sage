package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/config"
)

func stubAzureToken(t *testing.T, err error) {
	t.Helper()
	orig := newAzureTokenSource
	t.Cleanup(func() { newAzureTokenSource = orig })
	newAzureTokenSource = func() (azure.TokenSource, error) {
		if err != nil {
			return nil, err
		}
		return func(context.Context) (string, error) { return "tok", nil }, nil
	}
}

func azureCfg(sub, rg, server string) *config.Config {
	return &config.Config{Azure: config.AzureConfig{
		SubscriptionID: sub, ResourceGroup: rg, ServerName: server}}
}

func TestAzureManagedConfigOnlyForAzure(t *testing.T) {
	stubAzureToken(t, nil)
	adapter, note := azureManagedConfig(azureCfg("s", "rg", "srv"), "rds", "x.rds.amazonaws.com")
	if adapter != nil || note != "" {
		t.Fatalf("non-azure target got adapter=%v note=%q", adapter, note)
	}
}

func TestAzureManagedConfigNeedsIdentity(t *testing.T) {
	stubAzureToken(t, nil)
	adapter, note := azureManagedConfig(azureCfg("", "", ""), "azure",
		"srv-1.postgres.database.azure.com")
	if adapter != nil || !strings.Contains(note, "azure.subscription_id") {
		t.Fatalf("incomplete identity: adapter=%v note=%q", adapter, note)
	}
	adapter, note = azureManagedConfig(azureCfg("s", "rg", ""), "azure", "10.0.0.5")
	if adapter != nil || !strings.Contains(note, "azure.server_name") {
		t.Fatalf("no server name: adapter=%v note=%q", adapter, note)
	}
}

// The host-derived server name wins over azure.server_name, so one fleet
// config never points another database's parameters at the wrong server.
func TestAzureManagedConfigHostNameWins(t *testing.T) {
	stubAzureToken(t, nil)
	adapter, note := azureManagedConfig(azureCfg("s", "rg", "other-server"), "azure",
		"srv-1.postgres.database.azure.com")
	if adapter == nil || !strings.Contains(note, "srv-1") || strings.Contains(note, "other-server") {
		t.Fatalf("adapter=%v note=%q, want srv-1 from host", adapter, note)
	}
	adapter, note = azureManagedConfig(azureCfg("s", "rg", "private-srv"), "azure", "10.0.0.5")
	if adapter == nil || !strings.Contains(note, "private-srv") {
		t.Fatalf("private host fallback: adapter=%v note=%q", adapter, note)
	}
}

func TestAzureManagedConfigCredentialFailure(t *testing.T) {
	stubAzureToken(t, errors.New("no azure credential found"))
	adapter, note := azureManagedConfig(azureCfg("s", "rg", ""), "azure",
		"srv-1.postgres.database.azure.com")
	if adapter != nil || !strings.Contains(note, "no azure credential found") {
		t.Fatalf("credential failure: adapter=%v note=%q", adapter, note)
	}
}
