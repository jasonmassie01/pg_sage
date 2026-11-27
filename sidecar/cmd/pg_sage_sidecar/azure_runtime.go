package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/managedparam"
	prov "github.com/pg-sage/sidecar/internal/provider"
)

// newAzureTokenSource builds the ARM credential; a variable for tests.
var newAzureTokenSource = azure.DefaultTokenSource

// azureNoARM prefixes why a flexible server has no ARM adapter: proposals
// stay fill-in templates and the WAL-slot cap stays guidance-only.
const azureNoARM = "azure server parameters are not read over ARM (proposals are fill-in " +
	"templates; the WAL-slot cap is guidance-only): "

// azureManagedConfig returns the Azure server-parameter adapter for an
// Azure target, or nil with a note explaining why server parameters stay
// guidance-only. It returns nil and "" for other providers. The server name
// derived from the host wins over azure.server_name.
func azureManagedConfig(
	cfg *config.Config, provider, host string,
) (executor.ManagedConfigAdapter, string) {
	name := strings.ToLower(strings.TrimSpace(provider))
	switch {
	case name == "azure-single":
		return nil, "azure single server is retired; server parameters are guidance-only"
	case prov.Canonical(name) == prov.AzureCosmos:
		return nil, "azure cosmos db for postgresql: server parameters are guidance-only " +
			"(the ARM adapter targets flexible servers)"
	case prov.Canonical(name) != prov.Azure:
		return nil, ""
	}
	if cfg == nil {
		return nil, ""
	}
	server := azure.ServerNameFromHost(host)
	if server == "" {
		server = cfg.Azure.ServerName
	}
	switch {
	case cfg.Azure.SubscriptionID == "" || cfg.Azure.ResourceGroup == "":
		return nil, azureNoARM + "set azure.subscription_id and azure.resource_group"
	case server == "":
		return nil, azureNoARM + "host " + host +
			" does not name the server; set azure.server_name"
	}
	token, err := newAzureTokenSource()
	if err != nil {
		return nil, azureNoARM + err.Error()
	}
	adapter, err := azure.NewParameterAdapter(azure.Server{
		SubscriptionID: cfg.Azure.SubscriptionID,
		ResourceGroup:  cfg.Azure.ResourceGroup, Name: server,
	}, token)
	if err != nil {
		return nil, azureNoARM + err.Error()
	}
	return adapter, fmt.Sprintf("azure server %s: the WAL-slot cap (max_slot_wal_keep_size) "+
		"applies through ARM; other server parameters become operator-approved proposals "+
		"with the az command", server)
}

// azureTargetConfig is the config azureManagedConfig reads for one database:
// its cloud_resource.azure when set (validation requires all three names),
// else the process azure.* settings.
func azureTargetConfig(process *config.Config, r config.CloudResourceAzure) *config.Config {
	if r == (config.CloudResourceAzure{}) {
		return process
	}
	return &config.Config{Azure: config.AzureConfig{SubscriptionID: r.Subscription,
		ResourceGroup: r.ResourceGroup, ServerName: r.Server}}
}

// installAzureManagedConfig attaches the adapter to exec, logs the outcome
// and returns the adapter (nil when there is none) for the managed-change
// worker, which only reads server parameters through it.
func installAzureManagedConfig(
	exec *executor.Executor, cfg *config.Config, provider, host, scope string,
) executor.ManagedConfigAdapter {
	adapter, note := azureManagedConfig(cfg, provider, host)
	if note == "" {
		return nil
	}
	if adapter != nil {
		exec.WithManagedConfigAdapter(adapter)
	}
	logInfo(scope, "%s", note)
	return adapter
}

// managedChangeResolver is the managed-change worker's target: the cloud
// telemetry resolver (RDS, Aurora, Cloud SQL), else the Azure ARM adapter
// when it can describe server parameters, else nil (placeholders).
func managedChangeResolver(cloud managedparam.Resolver,
	adapter executor.ManagedConfigAdapter) managedparam.Resolver {
	if cloud != nil {
		return cloud
	}
	if r, ok := adapter.(managedparam.Resolver); ok {
		return r
	}
	return nil
}
