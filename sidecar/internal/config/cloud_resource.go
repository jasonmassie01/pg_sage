package config

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// CloudResourceConfig names the provider resource behind one database when
// its host does not (an IP, the Cloud SQL Auth Proxy on 127.0.0.1, an RDS
// Proxy or a custom DNS name). It names at most one cloud. A name only
// identifies the resource for telemetry and managed parameters; it never
// changes the provider the probes detected.
type CloudResourceConfig struct {
	AWS   CloudResourceAWS   `yaml:"aws"`
	GCP   CloudResourceGCP   `yaml:"gcp"`
	Azure CloudResourceAzure `yaml:"azure"`
}

// CloudResourceAWS names an RDS instance or an Aurora cluster.
type CloudResourceAWS struct {
	DBInstanceIdentifier string `yaml:"db_instance_identifier" doc:"RDS DB instance identifier of this database when its host does not name it (IP, RDS Proxy, custom DNS). An RDS endpoint host always wins."`
	ClusterIdentifier    string `yaml:"cluster_identifier" doc:"Aurora DB cluster identifier (its writer is read) when the host does not name it. Set this or db_instance_identifier, not both."`
	Region               string `yaml:"region" doc:"AWS region of the named instance or cluster. Empty: AWS_REGION."`
}

// CloudResourceGCP names a Cloud SQL instance or, with cluster, an AlloyDB
// instance.
type CloudResourceGCP struct {
	Project  string `yaml:"project" doc:"Google Cloud project of the Cloud SQL or AlloyDB instance. Empty: the project of a /cloudsql/<project>:<region>:<instance> socket host, else of the Application Default Credentials."`
	Instance string `yaml:"instance" doc:"Cloud SQL instance name when the host does not name it (Auth Proxy on 127.0.0.1, custom DNS). With it set the instance is read directly, never listed. For AlloyDB: the instance ID within cluster."`
	Cluster  string `yaml:"cluster" doc:"AlloyDB cluster ID of this database (set with instance). AlloyDB hosts never name the instance, so its host telemetry needs both."`
}

// CloudResourceAzure names an Azure Database for PostgreSQL flexible server.
type CloudResourceAzure struct {
	Subscription  string `yaml:"subscription" doc:"Azure subscription ID of this database's flexible server. Set with resource_group and server; wins over azure.* for this database."`
	ResourceGroup string `yaml:"resource_group" doc:"Azure resource group of this database's flexible server."`
	Server        string `yaml:"server" doc:"Flexible server name when the host does not name it. A <name>.postgres.database.azure.com host always wins."`
}

// ProviderOverrides are the values provider: accepts: every name provider
// detection can settle on except unknown, which is what an empty override
// already means.
var ProviderOverrides = []string{
	"self-managed", "aurora", "rds", "alloydb", "cloud-sql", "azure",
	"azure-cosmos", "lakebase", "neon", "supabase",
}

// cloudFamily is the providers a cloud_resource block may describe.
var cloudFamily = map[string][]string{
	"aws": {"aurora", "rds"}, "gcp": {"cloud-sql", "alloydb"}, "azure": {"azure"},
}

var (
	cloudAzureSubscription = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	cloudAzureResourceGroup = regexp.MustCompile(`^[A-Za-z0-9._()-]{0,89}[A-Za-z0-9_()-]$`)
	cloudAzureServer        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// Cloud is the one cloud the block names: "aws", "gcp", "azure" or "".
func (r CloudResourceConfig) Cloud() string {
	clouds := r.clouds()
	if len(clouds) == 1 {
		return clouds[0]
	}
	return ""
}

func (r CloudResourceConfig) clouds() []string {
	var out []string
	if r.AWS != (CloudResourceAWS{}) {
		out = append(out, "aws")
	}
	if r.GCP != (CloudResourceGCP{}) {
		out = append(out, "gcp")
	}
	if r.Azure != (CloudResourceAzure{}) {
		out = append(out, "azure")
	}
	return out
}

// validateCloudResources checks the provider override and resource of
// postgres: and of every databases[] entry.
func (c *Config) validateCloudResources() error {
	if err := validateCloudIdentity("postgres", c.Postgres.Provider,
		c.Postgres.CloudResource); err != nil {
		return err
	}
	if err := validateCNPG("postgres", c.Postgres.Provider, c.Postgres.CNPG,
		c.Postgres.CloudResource); err != nil {
		return err
	}
	for i, db := range c.Databases {
		field := fmt.Sprintf("databases[%d]", i)
		if db.Name != "" {
			field = fmt.Sprintf("databases[%q]", db.Name)
		}
		if err := validateCloudIdentity(field, db.Provider, db.CloudResource); err != nil {
			return err
		}
		if err := validateCNPG(field, db.Provider, db.CNPG, db.CloudResource); err != nil {
			return err
		}
	}
	return nil
}

func validateCloudIdentity(field, provider string, r CloudResourceConfig) error {
	if err := validateProviderOverride(field, provider); err != nil {
		return err
	}
	clouds := r.clouds()
	if len(clouds) > 1 {
		return fmt.Errorf("%s.cloud_resource names %s; name the resource of one cloud",
			field, strings.Join(clouds, " and "))
	}
	if len(clouds) == 0 {
		return nil
	}
	cloud := clouds[0]
	if provider != "" && !slices.Contains(cloudFamily[cloud], provider) {
		return fmt.Errorf("%s.cloud_resource.%s does not describe provider %q; %s "+
			"resources belong to %s", field, cloud, provider, cloud,
			strings.Join(cloudFamily[cloud], " or "))
	}
	switch cloud {
	case "aws":
		return validateAWSResource(field+".cloud_resource.aws", r.AWS)
	case "gcp":
		return validateGCPResource(field+".cloud_resource.gcp", provider, r.GCP)
	}
	return validateAzureResource(field+".cloud_resource.azure", r.Azure)
}

func validateProviderOverride(field, provider string) error {
	if provider == "" || slices.Contains(ProviderOverrides, provider) {
		return nil
	}
	return fmt.Errorf("%s.provider %q is not a provider pg_sage knows; use one of %s, "+
		"or leave it empty to rely on detection", field, provider,
		strings.Join(ProviderOverrides, ", "))
}

func validateAWSResource(field string, r CloudResourceAWS) error {
	if err := matchNames(field, map[string]nameCheck{
		"db_instance_identifier": {r.DBInstanceIdentifier, cloudRDSIdentifier},
		"cluster_identifier":     {r.ClusterIdentifier, cloudRDSIdentifier},
		"region":                 {r.Region, cloudAWSRegion},
	}); err != nil {
		return err
	}
	switch {
	case r.DBInstanceIdentifier != "" && r.ClusterIdentifier != "":
		return fmt.Errorf("%s: set db_instance_identifier or cluster_identifier, not both",
			field)
	case r.DBInstanceIdentifier == "" && r.ClusterIdentifier == "":
		return fmt.Errorf("%s names no resource; set db_instance_identifier or "+
			"cluster_identifier", field)
	}
	return nil
}

func validateGCPResource(field, provider string, r CloudResourceGCP) error {
	switch {
	case r.Cluster != "" && provider == "cloud-sql":
		return fmt.Errorf("%s.cluster names an AlloyDB cluster but the provider is "+
			"cloud-sql; drop cluster or set provider: alloydb", field)
	case (r.Cluster != "" || provider == "alloydb") && (r.Cluster == "" || r.Instance == ""):
		return fmt.Errorf("%s: an AlloyDB instance needs cluster and instance together", field)
	}
	return matchNames(field, map[string]nameCheck{
		"project":  {r.Project, cloudGCPProject},
		"instance": {r.Instance, cloudGCPInstance},
		"cluster":  {r.Cluster, cloudAlloyDBID},
	})
}

func validateAzureResource(field string, r CloudResourceAzure) error {
	if r.Subscription == "" || r.ResourceGroup == "" || r.Server == "" {
		return fmt.Errorf("%s needs subscription, resource_group and server together", field)
	}
	return matchNames(field, map[string]nameCheck{
		"subscription":   {strings.ToLower(r.Subscription), cloudAzureSubscription},
		"resource_group": {r.ResourceGroup, cloudAzureResourceGroup},
		"server":         {r.Server, cloudAzureServer},
	})
}

type nameCheck struct {
	value   string
	pattern *regexp.Regexp
}

// matchNames rejects the first set value that its pattern does not match,
// in key order so the error is deterministic.
func matchNames(field string, checks map[string]nameCheck) error {
	keys := make([]string, 0, len(checks))
	for key := range checks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		chk := checks[key]
		if chk.value != "" && !chk.pattern.MatchString(chk.value) {
			return fmt.Errorf("%s.%s %q is not a valid name", field, key, chk.value)
		}
	}
	return nil
}
