package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// newAzureTokenSource builds the ARM credential; a variable for tests.
var newAzureTokenSource = azure.DefaultTokenSource

// azureManagedConfig returns the Azure server-parameter adapter for an
// Azure target, or nil with a note explaining why server parameters stay
// guidance-only. It returns nil and "" for other providers. The server name
// derived from the host wins over azure.server_name.
func azureManagedConfig(
	cfg *config.Config, provider, host string,
) (executor.ManagedConfigAdapter, string) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "azure", "azure-flexible":
	case "azure-single":
		return nil, "azure single server is retired; server parameters are guidance-only"
	default:
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
		return nil, "azure server parameters are guidance-only: set " +
			"azure.subscription_id and azure.resource_group to apply them"
	case server == "":
		return nil, "azure server parameters are guidance-only: host " + host +
			" does not name the server; set azure.server_name"
	}
	token, err := newAzureTokenSource()
	if err != nil {
		return nil, "azure server parameters are guidance-only: " + err.Error()
	}
	adapter, err := azure.NewParameterAdapter(azure.Server{
		SubscriptionID: cfg.Azure.SubscriptionID,
		ResourceGroup:  cfg.Azure.ResourceGroup, Name: server,
	}, token)
	if err != nil {
		return nil, "azure server parameters are guidance-only: " + err.Error()
	}
	return adapter, fmt.Sprintf("azure server parameters apply through ARM for server %s", server)
}

// installAzureManagedConfig attaches the adapter to exec and logs the outcome.
func installAzureManagedConfig(
	exec *executor.Executor, cfg *config.Config, provider, host, scope string,
) {
	adapter, note := azureManagedConfig(cfg, provider, host)
	if note == "" {
		return
	}
	if adapter != nil {
		exec.WithManagedConfigAdapter(adapter)
	}
	logInfo(scope, "%s", note)
}
