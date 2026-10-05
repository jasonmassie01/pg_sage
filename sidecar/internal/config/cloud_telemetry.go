package config

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"time"
)

// CloudTelemetryConfig is managed-cloud host telemetry (roadmap phase 3):
// CloudWatch and Performance Insights for RDS/Aurora, Cloud Monitoring for
// Cloud SQL. It only reads provider metrics, instance metadata and
// parameters. Credentials come from the providers' standard chains (AWS
// default chain; Google Application Default Credentials), never from this
// file; without them telemetry reports unavailable and nothing changes.
type CloudTelemetryConfig struct {
	Enabled               *bool             `yaml:"enabled" doc:"Read host telemetry (CPU, memory, storage, IOPS, replica lag, DB load) and parameter groups / database flags of RDS, Aurora and Cloud SQL databases. Nil defaults to enabled; without provider credentials it reports unavailable."`
	PollIntervalSeconds   int               `yaml:"poll_interval_seconds" doc:"Seconds between telemetry polls. 0 = 60. Range 30-3600. CloudWatch bills GetMetricData per metric (about 10 metrics per poll)."`
	MaxReplicaLagSeconds  float64           `yaml:"max_replica_lag_seconds" doc:"Autonomous index builds wait while a replica lags more than this. 0 = 30."`
	MinFreeStoragePct     float64           `yaml:"min_free_storage_pct" doc:"Autonomous index builds wait while bounded free storage (including autoscaling headroom) is below this percentage. 0 = 10."`
	MinStorageRunwayHours float64           `yaml:"min_storage_runway_hours" doc:"Autonomous index builds wait while storage at its recent rate runs out within this many hours. 0 = 24."`
	MinAvailableMemoryPct float64           `yaml:"min_available_memory_pct" doc:"Autonomous index builds wait, and memory settings may not grow, while available host memory is below this percentage. 0 = 5."`
	AWS                   CloudTelemetryAWS `yaml:"aws"`
	GCP                   CloudTelemetryGCP `yaml:"gcp"`
}

// CloudTelemetryAWS names an RDS instance or Aurora cluster whose host
// does not (an IP, custom DNS). Standalone mode only.
type CloudTelemetryAWS struct {
	Region               string `yaml:"region" doc:"AWS region of the instance when the host does not name it (an RDS endpoint host always wins)."`
	DBInstanceIdentifier string `yaml:"db_instance_identifier" doc:"RDS DB instance identifier when the host is not an RDS endpoint. Standalone mode only."`
	DBClusterIdentifier  string `yaml:"db_cluster_identifier" doc:"Aurora DB cluster identifier (its writer is read) when the host is not an Aurora endpoint. Standalone mode only."`
}

// CloudTelemetryGCP names the Cloud SQL project and instance.
type CloudTelemetryGCP struct {
	Project  string `yaml:"project" doc:"Google Cloud project of the Cloud SQL instance. Empty: the project of the Application Default Credentials. Instances are matched by the host's IP address."`
	Instance string `yaml:"instance" doc:"Cloud SQL instance name when the host is not its IP address. Standalone mode only."`
}

var (
	cloudRDSIdentifier = regexp.MustCompile(`^[a-z](?:[a-z0-9]|-[a-z0-9]){0,62}$`)
	cloudAWSRegion     = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`)
	cloudGCPProject    = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	cloudGCPInstance   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,97}$`)
)

// IsEnabled reports whether telemetry runs (default: on).
func (c CloudTelemetryConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// PollInterval is the polling period.
func (c CloudTelemetryConfig) PollInterval() time.Duration {
	if c.PollIntervalSeconds <= 0 {
		return time.Minute
	}
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

func orDefault(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

// MaxReplicaLag is the replica-lag ceiling in seconds.
func (c CloudTelemetryConfig) MaxReplicaLag() float64 {
	return orDefault(c.MaxReplicaLagSeconds, 30)
}

// MinFreeStorage is the free-storage floor in percent.
func (c CloudTelemetryConfig) MinFreeStorage() float64 { return orDefault(c.MinFreeStoragePct, 10) }

// MinStorageRunway is the storage-runway floor in hours.
func (c CloudTelemetryConfig) MinStorageRunway() float64 {
	return orDefault(c.MinStorageRunwayHours, 24)
}

// MinAvailableMemory is the available-memory floor in percent.
func (c CloudTelemetryConfig) MinAvailableMemory() float64 {
	return orDefault(c.MinAvailableMemoryPct, 5)
}

// validateCloudTelemetry checks ranges and identifiers; explicit
// instance names are standalone-only (fleet databases are identified
// from their own hosts).
func (c *Config) validateCloudTelemetry() error {
	ct := c.CloudTelemetry
	if ct.PollIntervalSeconds != 0 && (ct.PollIntervalSeconds < 30 ||
		ct.PollIntervalSeconds > 3600) {
		return fmt.Errorf("cloud_telemetry.poll_interval_seconds must be 0 or 30-3600")
	}
	for name, v := range map[string]float64{"max_replica_lag_seconds": ct.MaxReplicaLagSeconds,
		"min_storage_runway_hours": ct.MinStorageRunwayHours,
		"min_free_storage_pct":     ct.MinFreeStoragePct,
		"min_available_memory_pct": ct.MinAvailableMemoryPct} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("cloud_telemetry.%s must be a finite value >= 0", name)
		}
	}
	if ct.MinFreeStoragePct > 100 || ct.MinAvailableMemoryPct > 100 {
		return fmt.Errorf("cloud_telemetry percentages must be at most 100")
	}
	return ct.validateIdentity(c.Mode)
}

func (ct CloudTelemetryConfig) validateIdentity(mode string) error {
	checks := []struct {
		field, value string
		pattern      *regexp.Regexp
	}{
		{"aws.region", ct.AWS.Region, cloudAWSRegion},
		{"aws.db_instance_identifier", ct.AWS.DBInstanceIdentifier, cloudRDSIdentifier},
		{"aws.db_cluster_identifier", ct.AWS.DBClusterIdentifier, cloudRDSIdentifier},
		{"gcp.project", ct.GCP.Project, cloudGCPProject},
		{"gcp.instance", ct.GCP.Instance, cloudGCPInstance},
	}
	for _, chk := range checks {
		if chk.value != "" && !chk.pattern.MatchString(chk.value) {
			return fmt.Errorf("cloud_telemetry.%s %q is not a valid name", chk.field, chk.value)
		}
	}
	named := ct.AWS.DBInstanceIdentifier != "" || ct.AWS.DBClusterIdentifier != "" ||
		ct.GCP.Instance != ""
	if named && mode != "" && mode != "standalone" {
		return fmt.Errorf("cloud_telemetry instance names are standalone only; fleet " +
			"databases are identified from their hosts")
	}
	if ct.AWS.DBInstanceIdentifier != "" && ct.AWS.DBClusterIdentifier != "" {
		return fmt.Errorf("cloud_telemetry.aws: set db_instance_identifier or " +
			"db_cluster_identifier, not both")
	}
	return nil
}

func overlayCloudTelemetryEnv(cfg *Config) {
	if v, ok := envBool("SAGE_CLOUD_TELEMETRY_ENABLED"); ok {
		cfg.CloudTelemetry.Enabled = &v
	}
	for env, field := range map[string]*string{
		"SAGE_AWS_REGION":                 &cfg.CloudTelemetry.AWS.Region,
		"SAGE_AWS_DB_INSTANCE_IDENTIFIER": &cfg.CloudTelemetry.AWS.DBInstanceIdentifier,
		"SAGE_AWS_DB_CLUSTER_IDENTIFIER":  &cfg.CloudTelemetry.AWS.DBClusterIdentifier,
		"SAGE_GCP_PROJECT":                &cfg.CloudTelemetry.GCP.Project,
		"SAGE_GCP_CLOUDSQL_INSTANCE":      &cfg.CloudTelemetry.GCP.Instance,
	} {
		if v := os.Getenv(env); v != "" {
			*field = v
		}
	}
}
