package main

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// Managed-cloud telemetry (roadmap phase 3): which provider resource a
// database is, and the credentials to read it. Resources are named by
// the connection host (an RDS/Aurora endpoint, a Cloud SQL IP) or, in
// standalone mode, by cloud_telemetry.*; nothing is ever discovered
// account-wide.

type gcpTokenSource interface {
	Token(ctx context.Context) (string, error)
	Project() string
}

// cloudDeps are the credential chains (variables for tests).
type cloudDeps struct {
	awsCredentials func(ctx context.Context, region string) (aws.CredentialsProvider, error)
	gcpToken       func(ctx context.Context) (gcpTokenSource, error)
}

func defaultCloudDeps() cloudDeps {
	return cloudDeps{
		awsCredentials: func(ctx context.Context, region string) (aws.CredentialsProvider,
			error) {
			c, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
			if err != nil {
				return nil, err
			}
			return c.Credentials, nil
		},
		gcpToken: func(ctx context.Context) (gcpTokenSource, error) {
			return cloudtel.NewGCPTokenSource(ctx, cloudtel.GCPAuthOptions{
				CredentialsFile: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")})
		},
	}
}

// cloudTelemetryFor builds the database's telemetry runtime and the
// managed-change resolver: nil for providers without telemetry or when it
// is disabled; an unavailable runtime (with the reason) when the resource
// or credentials cannot be established.
func cloudTelemetryFor(ctx context.Context, c *config.Config, provider, host,
	database string, standalone bool, deps cloudDeps) (*cloudtel.Runtime,
	managedparam.Resolver) {
	provider = managedparam.NormalizeProvider(provider)
	if c == nil || provider == "" || !c.CloudTelemetry.IsEnabled() {
		return nil, nil
	}
	var src interface {
		cloudtel.Source
		managedparam.Resolver
	}
	var err error
	if provider == "cloud-sql" {
		src, err = gcpSourceFor(ctx, c.CloudTelemetry, host, standalone, deps)
	} else {
		src, err = awsSourceFor(ctx, c.CloudTelemetry, host, standalone, deps)
	}
	if err != nil {
		return cloudtel.Unavailable(database, provider, err.Error()), nil
	}
	rt, err := cloudtel.NewRuntime(src, cloudtel.RuntimeOptions{Database: database,
		Interval: c.CloudTelemetry.PollInterval(), Limits: cloudLimits(c.CloudTelemetry)})
	if err != nil {
		return cloudtel.Unavailable(database, provider, err.Error()), nil
	}
	return rt, src
}

func cloudLimits(ct config.CloudTelemetryConfig) cloudtel.Limits {
	return cloudtel.Limits{MaxReplicaLagSeconds: ct.MaxReplicaLag(),
		MinFreeStoragePct: ct.MinFreeStorage(), MinStorageRunwayHours: ct.MinStorageRunway(),
		MinAvailableMemoryPct: ct.MinAvailableMemory()}
}

func awsSourceFor(ctx context.Context, ct config.CloudTelemetryConfig, host string,
	standalone bool, deps cloudDeps) (*cloudtel.AWSSource, error) {
	opts := cloudtel.AWSOptions{}
	if h, ok := cloudtel.ParseRDSHost(host); ok {
		opts.Region, opts.ReaderEndpoint = h.Region, h.Reader
		if h.Cluster {
			opts.ClusterID = h.Identifier
		} else {
			opts.InstanceID = h.Identifier
		}
	} else {
		named := ct.AWS.DBInstanceIdentifier != "" || ct.AWS.DBClusterIdentifier != ""
		if !standalone || !named {
			return nil, fmt.Errorf("host %s does not name an RDS instance or Aurora cluster; "+
				"connect through its endpoint or, in standalone mode, set "+
				"cloud_telemetry.aws.db_instance_identifier (or db_cluster_identifier) "+
				"and region", host)
		}
		opts.InstanceID, opts.ClusterID = ct.AWS.DBInstanceIdentifier, ct.AWS.DBClusterIdentifier
		opts.Region = ct.AWS.Region
		if opts.Region == "" {
			opts.Region = os.Getenv("AWS_REGION")
		}
	}
	creds, err := deps.awsCredentials(ctx, opts.Region)
	if err != nil {
		return nil, fmt.Errorf("AWS configuration: %w", err)
	}
	opts.Credentials = creds
	return cloudtel.NewAWSSource(opts)
}

func gcpSourceFor(ctx context.Context, ct config.CloudTelemetryConfig, host string,
	standalone bool, deps cloudDeps) (*cloudtel.GCPSource, error) {
	opts := cloudtel.GCPOptions{Project: ct.GCP.Project}
	switch {
	case standalone && ct.GCP.Instance != "":
		opts.Instance = ct.GCP.Instance
	case net.ParseIP(host) != nil:
		opts.HostIP = host
	default:
		return nil, fmt.Errorf("host %s is not an IP address and Cloud SQL instances are "+
			"matched by IP; connect by IP or, in standalone mode, set "+
			"cloud_telemetry.gcp.instance", host)
	}
	token, err := deps.gcpToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("google credentials: %w", err)
	}
	if opts.Project == "" {
		opts.Project = token.Project()
	}
	if opts.Project == "" {
		return nil, fmt.Errorf("the Google credentials name no project; set " +
			"cloud_telemetry.gcp.project")
	}
	opts.Token = token
	return cloudtel.NewGCPSource(opts)
}
