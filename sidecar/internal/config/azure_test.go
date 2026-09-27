package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAzureConfigFromYAMLAndEnv(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "azure:\n  subscription_id: sub-yaml\n  resource_group: rg-yaml\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Azure.SubscriptionID != "sub-yaml" || cfg.Azure.ResourceGroup != "rg-yaml" ||
		cfg.Azure.ServerName != "" {
		t.Fatalf("azure from yaml = %+v", cfg.Azure)
	}
	if cfg.Azure.Configured() {
		t.Fatal("azure without a server name reported configured before host fallback")
	}

	t.Setenv("SAGE_AZURE_SUBSCRIPTION_ID", "sub-env")
	t.Setenv("SAGE_AZURE_RESOURCE_GROUP", "rg-env")
	t.Setenv("SAGE_AZURE_SERVER_NAME", "srv-env")
	cfg, err = Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Azure != (AzureConfig{SubscriptionID: "sub-env", ResourceGroup: "rg-env",
		ServerName: "srv-env"}) || !cfg.Azure.Configured() {
		t.Fatalf("env must override yaml: %+v", cfg.Azure)
	}
}

func TestAzureConfigDefaultsEmpty(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Azure != (AzureConfig{}) || cfg.Azure.Configured() {
		t.Fatalf("default azure config = %+v, want empty", cfg.Azure)
	}
	if lifecycle, ok := LookupFieldLifecycle("azure.subscription_id"); !ok ||
		lifecycle.Lifecycle != LifecycleRestart {
		t.Fatalf("azure.subscription_id lifecycle = %+v, want restart", lifecycle)
	}
}
