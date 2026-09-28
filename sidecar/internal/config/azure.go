package config

import "os"

// AzureConfig identifies an Azure Database for PostgreSQL flexible server so
// pg_sage can apply server parameters through Azure Resource Manager, where
// ALTER SYSTEM is unavailable. Credentials come from the Azure identity
// chain (service-principal environment variables, managed identity, or the
// Azure CLI login), never from this file.
type AzureConfig struct {
	SubscriptionID string `yaml:"subscription_id" doc:"Azure subscription ID of the flexible server. With resource_group set, pg_sage applies server parameters through Azure Resource Manager."`
	ResourceGroup  string `yaml:"resource_group" doc:"Azure resource group that contains the flexible server."`
	ServerName     string `yaml:"server_name" doc:"Flexible server name for hosts that do not name it (private DNS or IP). A <name>.postgres.database.azure.com host always wins."`
}

// Configured reports whether every identity field is set.
func (a AzureConfig) Configured() bool {
	return a.SubscriptionID != "" && a.ResourceGroup != "" && a.ServerName != ""
}

func overlayAzureEnv(cfg *Config) {
	for env, field := range map[string]*string{
		"SAGE_AZURE_SUBSCRIPTION_ID": &cfg.Azure.SubscriptionID,
		"SAGE_AZURE_RESOURCE_GROUP":  &cfg.Azure.ResourceGroup,
		"SAGE_AZURE_SERVER_NAME":     &cfg.Azure.ServerName,
	} {
		if v := os.Getenv(env); v != "" {
			*field = v
		}
	}
}
