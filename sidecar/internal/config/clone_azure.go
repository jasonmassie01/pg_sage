package config

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	cloneAzureSubscription = regexp.MustCompile(
		`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	cloneAzureGroup  = regexp.MustCompile(`^[A-Za-z0-9._()-]{0,89}[A-Za-z0-9_()-]$`)
	cloneAzureServer = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

const azureRequired = "is required for clone.provider azure_restore"

// CloneAzureConfig is the Azure Database for PostgreSQL flexible server
// rehearsal clones are restored from (clone.provider azure_restore,
// lab-05g), in the same resource group and region. Azure credentials come
// from the DefaultAzureCredential chain.
type CloneAzureConfig struct {
	SubscriptionID string `yaml:"subscription_id" doc:"Azure subscription ID of the monitored flexible server; restores are made there."`
	ResourceGroup  string `yaml:"resource_group" doc:"Resource group of the monitored flexible server; restores are made in it and the reaper looks for leaked ones there."`
	Server         string `yaml:"server" doc:"Name of the monitored flexible server (not its FQDN); pg_sage must connect to its <name>.postgres.database.azure.com FQDN."`
}

// validateAzure checks azure_restore: a full copy needs
// clone.allow_full_copy, a subscription, a resource group, a server name
// and a restore deadline within bounds. It runs only when azure_restore is
// the provider.
func (c CloneProviderConfig) validateAzure() error {
	if !c.AllowFullCopy {
		return fmt.Errorf("clone.provider azure_restore restores a whole Azure flexible " +
			"server for each rehearsal (a full server bill while it lives): set " +
			"clone.allow_full_copy: true to opt in, or choose another clone.provider")
	}
	a := c.Azure
	switch {
	case a.SubscriptionID == "":
		return fmt.Errorf("clone.azure.subscription_id %s", azureRequired)
	case !cloneAzureSubscription.MatchString(a.SubscriptionID):
		return fmt.Errorf("clone.azure.subscription_id %q must be an Azure subscription ID "+
			"(a GUID)", a.SubscriptionID)
	case a.ResourceGroup == "":
		return fmt.Errorf("clone.azure.resource_group %s", azureRequired)
	case !cloneAzureGroup.MatchString(a.ResourceGroup) || strings.Contains(a.ResourceGroup, ".."):
		return fmt.Errorf("clone.azure.resource_group %q must be an Azure resource group "+
			"name (1-90 letters, digits, '.', '_', '-' or parentheses, not ending in '.')",
			a.ResourceGroup)
	case a.Server == "":
		return fmt.Errorf("clone.azure.server %s", azureRequired)
	case !cloneAzureServer.MatchString(a.Server) || clonePgSageName.MatchString(a.Server):
		return fmt.Errorf("clone.azure.server %q must be a flexible server name (3-63 "+
			"lowercase letters, digits and hyphens, not the FQDN) and not a pg_sage clone",
			a.Server)
	}
	return c.validateRestoreDeadline()
}
