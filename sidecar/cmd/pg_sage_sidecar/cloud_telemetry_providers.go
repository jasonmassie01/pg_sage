package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// azureSourceFor names the flexible server as the ARM adapter does: the
// database's cloud_resource.azure, else azure.*; a host that names the
// server wins over the configured name.
func azureSourceFor(c *config.Config, host string, r config.CloudResourceAzure,
	deps cloudDeps) (*cloudtel.AzureSource, error) {
	target := azureTargetConfig(c, r).Azure
	server := azure.ServerNameFromHost(host)
	if server == "" {
		server = target.ServerName
	}
	switch {
	case target.SubscriptionID == "" || target.ResourceGroup == "":
		return nil, errors.New("metrics from Azure Monitor need the server's resource ID; set " +
			"azure.subscription_id and azure.resource_group (or cloud_resource.azure on " +
			"this database)")
	case server == "":
		return nil, fmt.Errorf("host %s does not name the flexible server; set "+
			"azure.server_name (or cloud_resource.azure.server on this database)", host)
	}
	token, err := deps.azureToken()
	if err != nil {
		return nil, err
	}
	return cloudtel.NewAzureSource(cloudtel.AzureOptions{
		SubscriptionID: target.SubscriptionID, ResourceGroup: target.ResourceGroup,
		Server: server, Token: cloudtel.AzureTokenFunc(token)})
}

// alloyDBSourceFor names the AlloyDB instance: AlloyDB hosts (private IPs,
// the Auth Proxy on loopback) never name it, so cloud_resource.gcp.cluster
// and instance, or in standalone mode cloud_telemetry.alloydb, must.
func alloyDBSourceFor(ctx context.Context, ct config.CloudTelemetryConfig,
	r config.CloudResourceGCP, standalone bool,
	deps cloudDeps) (*cloudtel.AlloyDBSource, error) {
	opts := cloudtel.AlloyDBOptions{Project: r.Project, Cluster: r.Cluster,
		Instance: r.Instance}
	if opts.Cluster == "" && standalone {
		opts = cloudtel.AlloyDBOptions{Project: ct.AlloyDB.Project,
			Cluster: ct.AlloyDB.Cluster, Instance: ct.AlloyDB.Instance}
	}
	if opts.Cluster == "" || opts.Instance == "" {
		return nil, errors.New("AlloyDB hosts do not name the instance; set " +
			"cloud_resource.gcp.cluster and instance (and project) on this database or, in " +
			"standalone mode, cloud_telemetry.alloydb.cluster and instance")
	}
	token, err := deps.gcpToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("google credentials: %w", err)
	}
	if opts.Project == "" {
		opts.Project = token.Project()
	}
	if opts.Project == "" {
		return nil, errors.New("the Google credentials name no project; set " +
			"cloud_resource.gcp.project (or cloud_telemetry.alloydb.project)")
	}
	opts.Token = token
	return cloudtel.NewAlloyDBSource(opts)
}
