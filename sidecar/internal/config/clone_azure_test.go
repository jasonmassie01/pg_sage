package config

import (
	"strings"
	"testing"
)

// lab-05g: clone.provider azure_restore rehearses on a point-in-time
// restore of clone.azure.server in clone.azure.subscription_id and
// clone.azure.resource_group: a full copy, so it needs
// clone.allow_full_copy, and each restore waits at most
// clone.restore_deadline_minutes.

const cloneAzureSub = "0f1e2d3c-4b5a-4697-8877-665544332211"

func azureRestoreConfig() *Config {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "azure_restore"
	cfg.Clone.AllowFullCopy = true
	cfg.Clone.Azure = CloneAzureConfig{SubscriptionID: cloneAzureSub,
		ResourceGroup: "rg-shop-prod", Server: "shop"}
	return cfg
}

func TestCloneAzureDefaults(t *testing.T) {
	c := DefaultConfig().Clone
	if c.Azure != (CloneAzureConfig{}) ||
		c.RestoreDeadlineMinutes != DefaultCloneRestoreDeadlineMinutes {
		t.Fatalf("defaults: azure %+v, deadline %d", c.Azure, c.RestoreDeadlineMinutes)
	}
	if err := azureRestoreConfig().validateAgentNative(); err != nil {
		t.Fatalf("azure_restore with a subscription, group, server and the opt-in: %v", err)
	}
}

// Invalid configuration: a full copy without the opt-in stops startup.
func TestCloneAzureNeedsAllowFullCopy(t *testing.T) {
	cfg := azureRestoreConfig()
	cfg.Clone.AllowFullCopy = false
	err := cfg.validateAgentNative()
	if err == nil || !strings.Contains(err.Error(), "clone.allow_full_copy") ||
		!strings.Contains(err.Error(), "azure_restore") {
		t.Fatalf("azure_restore without allow_full_copy = %v", err)
	}
}

func TestCloneAzureWarnsOfTheFullServerCost(t *testing.T) {
	w := azureRestoreConfig().Clone.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "azure_restore") ||
		!strings.Contains(w[0], "full") || !strings.Contains(w[0], "instance-hours") {
		t.Fatalf("warnings = %q, want one cost warning", w)
	}
}

// Default value masking: a block naming only the server keeps the
// restore deadline default instead of zero.
func TestCloneAzureYAMLKeepsDefaults(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: shop.postgres.database.azure.com
  user: app
  database: shop
clone:
  provider: azure_restore
  allow_full_copy: true
  azure:
    subscription_id: `+cloneAzureSub+`
    resource_group: rg-shop-prod
    server: shop
`)
	c := cfg.Clone
	if c.Provider != "azure_restore" || c.Azure != (CloneAzureConfig{
		SubscriptionID: cloneAzureSub, ResourceGroup: "rg-shop-prod", Server: "shop"}) ||
		c.RestoreDeadlineMinutes != 120 || !c.AllowFullCopy {
		t.Fatalf("loaded clone config = %+v", c)
	}
}

func TestCloneAzureValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*CloneAzureConfig)
		want   string
	}{
		"no subscription": {func(a *CloneAzureConfig) { a.SubscriptionID = "" },
			"subscription_id is required"},
		"bad subscription": {func(a *CloneAzureConfig) { a.SubscriptionID = "acme" },
			"subscription ID"},
		"no group": {func(a *CloneAzureConfig) { a.ResourceGroup = "" },
			"resource_group is required"},
		"bad group": {func(a *CloneAzureConfig) { a.ResourceGroup = "rg/x" }, "resource group"},
		"dot end":   {func(a *CloneAzureConfig) { a.ResourceGroup = "rg." }, "resource group"},
		"dot dot":   {func(a *CloneAzureConfig) { a.ResourceGroup = "rg..x" }, "resource group"},
		"long group": {func(a *CloneAzureConfig) { a.ResourceGroup = strings.Repeat("a", 91) },
			"resource group"},
		"no server":   {func(a *CloneAzureConfig) { a.Server = "" }, "server is required"},
		"bad server":  {func(a *CloneAzureConfig) { a.Server = "Shop" }, "server name"},
		"two letters": {func(a *CloneAzureConfig) { a.Server = "ab" }, "server name"},
		"long server": {func(a *CloneAzureConfig) { a.Server = strings.Repeat("a", 64) },
			"server name"},
		"an FQDN": {func(a *CloneAzureConfig) { a.Server = "shop.postgres.database.azure.com" },
			"server name"},
		"clone name": {func(a *CloneAzureConfig) { a.Server = "sage-rehearsal-0a1b2c3d" },
			"pg_sage clone"},
	}
	for name, tc := range cases {
		cfg := azureRestoreConfig()
		tc.mutate(&cfg.Clone.Azure)
		err := cfg.validateAgentNative()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	for d, ok := range map[int]bool{0: false, 29: false, 30: true, 720: true, 721: false} {
		cfg := azureRestoreConfig()
		cfg.Clone.RestoreDeadlineMinutes = d
		if err := cfg.validateAgentNative(); (err == nil) != ok {
			t.Errorf("deadline %d: err = %v, want accepted %v", d, err, ok)
		}
	}
	cfg := azureRestoreConfig()
	cfg.Clone.Azure.Server = strings.Repeat("a", 63)
	cfg.Clone.Azure.ResourceGroup = strings.Repeat("R", 90)
	cfg.Clone.Azure.SubscriptionID = strings.ToUpper(cloneAzureSub)
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("63-character server, 90-character group, uppercase subscription: %v", err)
	}
}

// A leftover clone.azure block never stops another provider.
func TestCloneAzureBlockIgnoredForOtherProviders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.Azure = CloneAzureConfig{SubscriptionID: "x", ResourceGroup: "a/b",
		Server: "Not/A/Server"}
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("provider none with a broken azure block: %v", err)
	}
}
