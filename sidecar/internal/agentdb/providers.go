package agentdb

import (
	"context"
	"strconv"
	"strings"
)

func BuildProvisionPlan(req RegisterRequest, profile SizeProfile) (ProvisionPlan, error) {
	normalizeProviderFields(&req)
	if req.Provider == ProviderLocalPostgres {
		return ProvisionPlan{}, ErrInvalid
	}
	if req.ProvisioningLevel != LevelInstance {
		return ProvisionPlan{}, ErrInvalid
	}
	if req.DatabaseName == "" {
		req.DatabaseName = resourceName(req.DeploymentID)
	}
	switch req.Provider {
	case ProviderAWSRDS:
		return awsRDSPlan(req, profile), nil
	case ProviderGCPCloudSQL:
		return cloudSQLPlan(req, profile), nil
	case ProviderDatabricksLakebase:
		return lakebasePlan(req, profile), nil
	default:
		return ProvisionPlan{}, ErrInvalid
	}
}

func ProviderReadinessList(ctx context.Context) []ProviderReadiness {
	return []ProviderReadiness{
		{
			Provider:  ProviderLocalPostgres,
			Label:     "Local Postgres",
			Interface: "pg_sage",
			Found:     true,
			Detail:    "uses active pg_sage PostgreSQL connection",
		},
		readiness(ctx, ProviderAWSRDS, "AWS RDS", "terraform_or_aws_sdk",
			"Terraform plan or AWS SDK client credentials required at execution boundary"),
		readiness(ctx, ProviderGCPCloudSQL, "GCP Cloud SQL", "terraform_or_cloudsql_admin_api",
			"Terraform plan or Cloud SQL Admin API credentials required at execution boundary"),
		readiness(ctx, ProviderDatabricksLakebase, "Databricks Lakebase",
			"databricks_api_or_terraform",
			"Databricks API credentials or Terraform provider support required at execution boundary"),
	}
}

func normalizeProviderFields(req *RegisterRequest) {
	req.Provider = normalizeProvider(req.Provider)
	req.ProvisioningLevel = normalizeProvisioningLevel(
		firstNonEmpty(req.ProvisioningLevel, req.IsolationType),
	)
	req.IsolationType = req.ProvisioningLevel
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func normalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return ProviderLocalPostgres
	}
	return provider
}

func normalizeProvisioningLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return LevelSchema
	}
	return level
}

func validProvider(provider string) bool {
	switch normalizeProvider(provider) {
	case ProviderLocalPostgres, ProviderAWSRDS, ProviderGCPCloudSQL,
		ProviderDatabricksLakebase:
		return true
	default:
		return false
	}
}

func validLevel(level string) bool {
	switch normalizeProvisioningLevel(level) {
	case LevelSchema, LevelDatabase, LevelInstance:
		return true
	default:
		return false
	}
}

func cloudProvider(provider string) bool {
	return normalizeProvider(provider) != ProviderLocalPostgres
}

func awsRDSPlan(req RegisterRequest, profile SizeProfile) ProvisionPlan {
	id := resourceName(req.DeploymentID)
	args := []string{
		"terraform", "apply", "-auto-approve",
		"-var", "provider=aws_rds",
		"-var", "db_instance_identifier=" + id,
		"-var", "engine=postgres",
		"-var", "db_instance_class=" + param(profile, "db_instance_class", "db.t4g.micro"),
		"-var", "allocated_storage=" + intParam(profile, "allocated_storage", profile.StorageGB, "20"),
		"-var", "database_name=" + req.DatabaseName,
		"-var", "backup_retention_days=" + param(profile, "backup_retention_days", "7"),
	}
	if region := param(profile, "region", ""); region != "" {
		args = append(args, "-var", "region="+region)
	}
	return plan(req, "terraform", args,
		"RDS instance creation uses Terraform or AWS SDK credentials; do not use provider CLI")
}

func cloudSQLPlan(req RegisterRequest, profile SizeProfile) ProvisionPlan {
	id := resourceName(req.DeploymentID)
	args := []string{
		"terraform", "apply", "-auto-approve",
		"-var", "provider=gcp_cloudsql",
		"-var", "instance_name=" + id,
		"-var", "database_version=" + param(profile, "database_version", "POSTGRES_16"),
		"-var", "tier=" + param(profile, "tier", "db-custom-1-3840"),
		"-var", "edition=" + param(profile, "edition", "ENTERPRISE"),
		"-var", "storage_size=" + intParam(profile, "storage_size", profile.StorageGB, "20"),
		"-var", "ipv4_enabled=" + boolParam(profile, "ipv4_enabled", true),
		"-var", "require_ssl=" + boolParam(profile, "require_ssl", true),
	}
	if region := param(profile, "region", ""); region != "" {
		args = append(args, "-var", "region="+region)
	}
	if project := param(profile, "project", ""); project != "" {
		args = append(args, "-var", "project="+project)
	}
	return plan(req, "terraform", args,
		"Cloud SQL instance creation uses Terraform or Google Cloud SQL Admin API; do not use gcloud")
}

func lakebasePlan(req RegisterRequest, profile SizeProfile) ProvisionPlan {
	id := resourceName(req.DeploymentID)
	mode := param(profile, "mode", "autoscaling_branch")
	if override := metadataString(req.Metadata, "lakebase_mode"); override != "" {
		mode = override
	}
	args := []string{"cloud_api", "databricks_lakebase"}
	if mode == "provisioned_instance" {
		args = append(args, "create_instance", id)
	} else {
		args = append(args, "create_branch", id)
		if project := param(profile, "project", ""); project != "" {
			args = append(args, "project="+project)
		}
	}
	out := plan(req, "cloud_api", args,
		"Lakebase provisioning uses Databricks APIs or Terraform provider support; do not use databricks CLI")
	out.Notes = append(out.Notes,
		"Lakebase extension allowlist should be verified before enabling "+
			"agent workloads that depend on pgvector, PostGIS, pg_hint_plan, "+
			"or pg_stat_statements.",
		"Lakebase is managed Postgres: use session, database, or role-level "+
			"parameters where allowed; do not plan instance-level GUC changes.",
	)
	return out
}

func plan(req RegisterRequest, tool string, args []string, note string) ProvisionPlan {
	return ProvisionPlan{
		Provider:      req.Provider,
		Level:         req.ProvisioningLevel,
		ExecutionMode: "manual_review",
		Commands:      []ProviderCommand{{Tool: tool, Args: args}},
		Notes:         []string{note},
	}
}

func planMap(plan ProvisionPlan) map[string]any {
	commands := make([]any, 0, len(plan.Commands))
	for _, command := range plan.Commands {
		commands = append(commands, map[string]any{
			"tool": command.Tool,
			"args": command.Args,
		})
	}
	return map[string]any{
		"provider":           plan.Provider,
		"provisioning_level": plan.Level,
		"execution_mode":     plan.ExecutionMode,
		"commands":           commands,
		"notes":              plan.Notes,
	}
}

func providerLifecycleCommand(dep Deployment, action string) (ProviderCommand, error) {
	if dep.Provider == ProviderLocalPostgres || dep.ProvisioningLevel != LevelInstance {
		return ProviderCommand{}, ErrInvalid
	}
	id := resourceName(dep.DeploymentID)
	switch dep.Provider {
	case ProviderAWSRDS:
		return awsRDSLifecycleCommand(id, action)
	case ProviderGCPCloudSQL:
		return cloudSQLLifecycleCommand(id, action)
	case ProviderDatabricksLakebase:
		return lakebaseLifecycleCommand(dep, id, action)
	default:
		return ProviderCommand{}, ErrInvalid
	}
}

func awsRDSLifecycleCommand(id string, action string) (ProviderCommand, error) {
	switch action {
	case "status":
		return ProviderCommand{Tool: "cloud_api", Args: []string{
			"cloud_api", "aws_rds", "describe_instance", id,
		}}, nil
	case "destroy":
		return ProviderCommand{Tool: "terraform", Args: []string{
			"terraform", "destroy", "-auto-approve",
			"-var", "provider=aws_rds",
			"-var", "db_instance_identifier=" + id,
		}}, nil
	default:
		return ProviderCommand{}, ErrInvalid
	}
}

func cloudSQLLifecycleCommand(id string, action string) (ProviderCommand, error) {
	switch action {
	case "status":
		return ProviderCommand{Tool: "cloud_api", Args: []string{
			"cloud_api", "gcp_cloudsql", "describe_instance", id,
		}}, nil
	case "destroy":
		return ProviderCommand{Tool: "terraform", Args: []string{
			"terraform", "destroy", "-auto-approve",
			"-var", "provider=gcp_cloudsql",
			"-var", "instance_name=" + id,
		}}, nil
	default:
		return ProviderCommand{}, ErrInvalid
	}
}

func lakebaseLifecycleCommand(
	dep Deployment,
	id string,
	action string,
) (ProviderCommand, error) {
	resource := "branches"
	if lakebasePlanUsesInstances(dep.ProvisioningPlan) {
		resource = "instances"
	}
	switch action {
	case "status":
		return ProviderCommand{Tool: "cloud_api", Args: []string{
			"cloud_api", "databricks_lakebase", "get_" + resource, id,
		}}, nil
	case "destroy":
		return ProviderCommand{Tool: "cloud_api", Args: []string{
			"cloud_api", "databricks_lakebase", "delete_" + resource, id,
		}}, nil
	default:
		return ProviderCommand{}, ErrInvalid
	}
}

func lakebasePlanUsesInstances(plan map[string]any) bool {
	commands, err := commandsFromPlan(plan)
	if err != nil || len(commands) == 0 {
		return false
	}
	for _, arg := range commands[0].Args {
		if arg == "instances" || arg == "create_instance" {
			return true
		}
	}
	return false
}

func providerBackupCheckCommand(dep Deployment) (ProviderCommand, string, error) {
	id := resourceName(dep.DeploymentID)
	switch dep.Provider {
	case ProviderLocalPostgres:
		return localBackupCheckCommand(dep)
	case ProviderAWSRDS:
		return ProviderCommand{Tool: "cloud_api", Args: []string{
			"cloud_api", "aws_rds", "describe_instance", id,
		}}, "managed_provider", nil
	case ProviderGCPCloudSQL:
		return ProviderCommand{Tool: "cloud_api", Args: []string{
			"cloud_api", "gcp_cloudsql", "describe_backup_config", id,
		}}, "managed_provider", nil
	case ProviderDatabricksLakebase:
		command, err := lakebaseLifecycleCommand(dep, id, "status")
		return command, "managed_provider", err
	default:
		return ProviderCommand{}, "", ErrInvalid
	}
}

func providerRestoreDrillCommand(
	dep Deployment,
	backups []Backup,
) (ProviderCommand, error) {
	id := resourceName(dep.DeploymentID)
	switch dep.Provider {
	case ProviderLocalPostgres:
		return ProviderCommand{Tool: "pg_restore", Args: []string{
			"pg_restore", "--list", restoreArchive(backups),
		}}, nil
	case ProviderAWSRDS:
		return ProviderCommand{Tool: "cloud_api", Args: []string{
			"cloud_api", "aws_rds", "describe_snapshots", id,
		}}, nil
	case ProviderGCPCloudSQL:
		return ProviderCommand{Tool: "cloud_api", Args: []string{
			"cloud_api", "gcp_cloudsql", "list_backups", id,
		}}, nil
	case ProviderDatabricksLakebase:
		return lakebaseLifecycleCommand(dep, id, "status")
	default:
		return ProviderCommand{}, ErrInvalid
	}
}

func localBackupCheckCommand(dep Deployment) (ProviderCommand, string, error) {
	switch dep.ProvisioningLevel {
	case LevelSchema:
		schema := dep.SchemaName
		if schema == "" {
			schema = sanitizeSchemaName(dep.DeploymentID)
		}
		return ProviderCommand{Tool: "pg_dump", Args: []string{
			"pg_dump", "--schema-only", "--schema", schema,
		}}, "self_managed", nil
	case LevelDatabase:
		db := dep.DatabaseName
		if db == "" {
			db = sanitizeDatabaseName(dep.DeploymentID)
		}
		return ProviderCommand{Tool: "pg_dump", Args: []string{
			"pg_dump", "--schema-only", "--dbname", db,
		}}, "self_managed", nil
	default:
		return ProviderCommand{}, "", ErrInvalid
	}
}

func restoreArchive(backups []Backup) string {
	for _, backup := range backups {
		if backup.ArchiveURI != "" {
			return backup.ArchiveURI
		}
	}
	return "agentdb-restore-placeholder.dump"
}

func readiness(_ context.Context, provider, label, iface, detail string) ProviderReadiness {
	return ProviderReadiness{
		Provider:  provider,
		Label:     label,
		Interface: iface,
		Found:     true,
		Detail:    detail,
	}
}

func param(profile SizeProfile, key, fallback string) string {
	if profile.ProviderParams != nil {
		if value, ok := profile.ProviderParams[key].(string); ok && value != "" {
			return value
		}
	}
	return fallback
}

func intParam(profile SizeProfile, key string, numeric float64, fallback string) string {
	if value := param(profile, key, ""); value != "" {
		return value
	}
	if numeric > 0 {
		return strings.TrimSuffix(
			strings.TrimSuffix(strconv.FormatFloat(numeric, 'f', 2, 64), "0"),
			".",
		)
	}
	return fallback
}

func boolParam(profile SizeProfile, key string, fallback bool) string {
	if profile.ProviderParams != nil {
		switch value := profile.ProviderParams[key].(type) {
		case bool:
			return strconv.FormatBool(value)
		case string:
			value = strings.TrimSpace(value)
			if value != "" {
				return value
			}
		}
	}
	return strconv.FormatBool(fallback)
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func resourceName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "agentdb"
	}
	return out
}
