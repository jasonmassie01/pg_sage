package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/pg-sage/sidecar/internal/azure"
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
	azureToken     func() (azure.TokenSource, error)
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
		azureToken: func() (azure.TokenSource, error) { return newAzureTokenSource() },
	}
}

// cloudTelemetryFor builds the database's telemetry runtime and, where
// the telemetry source also reads parameters (RDS, Aurora, Cloud SQL), the
// managed-change resolver: nil for providers without telemetry or when it
// is disabled; an unavailable runtime (with the reason) when the resource
// or credentials cannot be established. A host that names the resource
// always wins over the configured resource.
func cloudTelemetryFor(ctx context.Context, c *config.Config, provider, host,
	database string, standalone bool, resource config.CloudResourceConfig,
	deps cloudDeps) (*cloudtel.Runtime, managedparam.Resolver) {
	provider = managedparam.NormalizeProvider(provider)
	if c == nil || provider == "" || !c.CloudTelemetry.IsEnabled() {
		return nil, nil
	}
	src, resolver, err := cloudSourceFor(ctx, c, provider, host, standalone, resource, deps)
	if err != nil {
		return cloudtel.Unavailable(database, provider, err.Error()), nil
	}
	if src == nil {
		return nil, nil
	}
	rt, err := cloudtel.NewRuntime(src, cloudtel.RuntimeOptions{Database: database,
		Interval: c.CloudTelemetry.PollInterval(), Limits: cloudLimits(c.CloudTelemetry)})
	if err != nil {
		return cloudtel.Unavailable(database, provider, err.Error()), nil
	}
	return rt, resolver
}

// cloudSourceFor is the provider's telemetry source and its optional
// managed-change resolver (nil, nil, nil: no telemetry for provider).
func cloudSourceFor(ctx context.Context, c *config.Config, provider, host string,
	standalone bool, resource config.CloudResourceConfig,
	deps cloudDeps) (cloudtel.Source, managedparam.Resolver, error) {
	switch provider {
	case "cloud-sql":
		s, err := gcpSourceFor(ctx, c.CloudTelemetry, resource.GCP, host, standalone, deps)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	case "rds", "aurora":
		s, err := awsSourceFor(ctx, c, resource.AWS, provider, host, standalone, deps)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	case "azure": // managed changes resolve through the ARM parameter adapter
		s, err := azureSourceFor(c, host, resource.Azure, deps)
		if err != nil {
			return nil, nil, err
		}
		return s, nil, nil
	case "alloydb": // managed changes stay proposal-only
		s, err := alloyDBSourceFor(ctx, c.CloudTelemetry, resource.GCP, standalone, deps)
		if err != nil {
			return nil, nil, err
		}
		return s, nil, nil
	}
	return nil, nil, nil
}

func cloudLimits(ct config.CloudTelemetryConfig) cloudtel.Limits {
	return cloudtel.Limits{MaxReplicaLagSeconds: ct.MaxReplicaLag(),
		MinFreeStoragePct: ct.MinFreeStorage(), MinStorageRunwayHours: ct.MinStorageRunway(),
		MinAvailableMemoryPct: ct.MinAvailableMemory()}
}

// awsSourceFor names the RDS instance or Aurora cluster; provider is the
// normalized platform PostgreSQL reported, so an Aurora writer reached by
// its instance endpoint is Aurora. A manual snapshot counts as surviving a
// delete within backup.max_age_hours (drill-01b).
func awsSourceFor(ctx context.Context, c *config.Config, r config.CloudResourceAWS,
	provider, host string, standalone bool, deps cloudDeps) (*cloudtel.AWSSource, error) {
	ct := c.CloudTelemetry
	opts := cloudtel.AWSOptions{DetectedProvider: provider,
		IsolationMaxAge: c.Backup.MaxAge()}
	if h, ok := cloudtel.ParseRDSHost(host); ok {
		opts.Region, opts.ReaderEndpoint = h.Region, h.Reader
		if h.Cluster {
			opts.ClusterID = h.Identifier
		} else {
			opts.InstanceID = h.Identifier
		}
	} else {
		named, err := awsNamedResource(ct, r, host, standalone)
		if err != nil {
			return nil, err
		}
		opts.InstanceID, opts.ClusterID = named.DBInstanceIdentifier, named.ClusterIdentifier
		opts.Region = named.Region
	}
	creds, err := deps.awsCredentials(ctx, opts.Region)
	if err != nil {
		return nil, fmt.Errorf("AWS configuration: %w", err)
	}
	opts.Credentials = creds
	return cloudtel.NewAWSSource(opts)
}

// awsNamedResource is the database's cloud_resource.aws or, in standalone
// mode, the older cloud_telemetry.aws names, with the region filled in.
func awsNamedResource(ct config.CloudTelemetryConfig, r config.CloudResourceAWS,
	host string, standalone bool) (config.CloudResourceAWS, error) {
	legacy := ct.AWS.DBInstanceIdentifier != "" || ct.AWS.DBClusterIdentifier != ""
	switch {
	case r.DBInstanceIdentifier != "" || r.ClusterIdentifier != "":
	case standalone && legacy:
		r = config.CloudResourceAWS{DBInstanceIdentifier: ct.AWS.DBInstanceIdentifier,
			ClusterIdentifier: ct.AWS.DBClusterIdentifier}
	default:
		return r, fmt.Errorf("host %s does not name an RDS instance or Aurora cluster "+
			"(an IP, RDS Proxy or custom DNS name); set cloud_resource.aws."+
			"db_instance_identifier (or cluster_identifier) and region on this database "+
			"or, in standalone mode, cloud_telemetry.aws.db_instance_identifier", host)
	}
	if r.Region == "" {
		r.Region = ct.AWS.Region
	}
	if r.Region == "" {
		r.Region = os.Getenv("AWS_REGION")
	}
	return r, nil
}

func gcpSourceFor(ctx context.Context, ct config.CloudTelemetryConfig,
	r config.CloudResourceGCP, host string, standalone bool,
	deps cloudDeps) (*cloudtel.GCPSource, error) {
	opts, err := gcpOptionsFor(ct, r, host, standalone)
	if err != nil {
		return nil, err
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
			"cloud_resource.gcp.project (or cloud_telemetry.gcp.project)")
	}
	opts.Token = token
	return cloudtel.NewGCPSource(opts)
}

// gcpOptionsFor names the Cloud SQL instance without calling Google: an Auth
// Proxy socket host names it; else the configured instance; else the host
// IP, which is listed once to find the instance. Loopback never matches an
// instance, so it is refused before any API call.
func gcpOptionsFor(ct config.CloudTelemetryConfig, r config.CloudResourceGCP, host string,
	standalone bool) (cloudtel.GCPOptions, error) {
	opts := cloudtel.GCPOptions{Project: r.Project}
	if opts.Project == "" {
		opts.Project = ct.GCP.Project
	}
	if s, ok := cloudtel.ParseCloudSQLSocket(host); ok {
		opts.Project, opts.Instance = s.Project, s.Instance
		return opts, nil
	}
	switch {
	case r.Instance != "":
		opts.Instance = r.Instance
	case standalone && ct.GCP.Instance != "":
		opts.Instance = ct.GCP.Instance
	case isLoopbackHost(host):
		return opts, fmt.Errorf("host %s is loopback, as for the Cloud SQL Auth Proxy, and "+
			"its address never matches an instance; set cloud_resource.gcp.instance (and "+
			"project) on this database, or connect through the proxy's "+
			"/cloudsql/<project>:<region>:<instance> unix socket", host)
	case net.ParseIP(host) != nil:
		opts.HostIP = host
	case strings.HasPrefix(host, "/"):
		return opts, fmt.Errorf("unix socket %s does not name a Cloud SQL instance as "+
			"/cloudsql/<project>:<region>:<instance>; set cloud_resource.gcp.instance", host)
	default:
		return opts, fmt.Errorf("host %s is not an IP address and Cloud SQL instances are "+
			"matched by IP; connect by IP or set cloud_resource.gcp.instance (in "+
			"standalone mode cloud_telemetry.gcp.instance also works)", host)
	}
	return opts, nil
}
