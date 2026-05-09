package agentdb

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
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
			Provider: ProviderLocalPostgres,
			Label:    "Local Postgres",
			CLI:      "pg_sage",
			Found:    true,
			Detail:   "uses active pg_sage PostgreSQL connection",
		},
		readiness(ctx, ProviderAWSRDS, "AWS RDS", "aws"),
		readiness(ctx, ProviderGCPCloudSQL, "GCP Cloud SQL", "gcloud"),
		readiness(ctx, ProviderDatabricksLakebase, "Databricks Lakebase", "databricks"),
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
		"aws", "rds", "create-db-instance",
		"--db-instance-identifier", id,
		"--engine", "postgres",
		"--db-instance-class", param(profile, "db_instance_class", "db.t4g.micro"),
		"--allocated-storage", intParam(profile, "allocated_storage", profile.StorageGB, "20"),
		"--db-name", req.DatabaseName,
		"--backup-retention-period", param(profile, "backup_retention_days", "7"),
	}
	if region := param(profile, "region", ""); region != "" {
		args = append(args, "--region", region)
	}
	return plan(req, "aws", args, "RDS instance creation requires AWS credentials")
}

func cloudSQLPlan(req RegisterRequest, profile SizeProfile) ProvisionPlan {
	id := resourceName(req.DeploymentID)
	args := []string{
		"gcloud", "sql", "instances", "create", id,
		"--database-version", param(profile, "database_version", "POSTGRES_16"),
		"--tier", param(profile, "tier", "db-custom-1-3840"),
		"--storage-size", intParam(profile, "storage_size", profile.StorageGB, "20"),
	}
	if region := param(profile, "region", ""); region != "" {
		args = append(args, "--region", region)
	}
	if project := param(profile, "project", ""); project != "" {
		args = append(args, "--project", project)
	}
	return plan(req, "gcloud", args, "Cloud SQL instance creation requires gcloud auth")
}

func lakebasePlan(req RegisterRequest, profile SizeProfile) ProvisionPlan {
	id := resourceName(req.DeploymentID)
	mode := param(profile, "mode", "autoscaling_branch")
	args := []string{"databricks", "database"}
	if mode == "provisioned_instance" {
		args = append(args, "instances", "create", id)
	} else {
		args = append(args, "branches", "create", id)
		if project := param(profile, "project", ""); project != "" {
			args = append(args, "--project", project)
		}
	}
	out := plan(req, "databricks", args, "Lakebase uses Databricks database CLI")
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
		return ProviderCommand{Tool: "aws", Args: []string{
			"aws", "rds", "describe-db-instances",
			"--db-instance-identifier", id,
		}}, nil
	case "destroy":
		return ProviderCommand{Tool: "aws", Args: []string{
			"aws", "rds", "delete-db-instance",
			"--db-instance-identifier", id,
			"--skip-final-snapshot",
		}}, nil
	default:
		return ProviderCommand{}, ErrInvalid
	}
}

func cloudSQLLifecycleCommand(id string, action string) (ProviderCommand, error) {
	switch action {
	case "status":
		return ProviderCommand{Tool: "gcloud", Args: []string{
			"gcloud", "sql", "instances", "describe", id,
		}}, nil
	case "destroy":
		return ProviderCommand{Tool: "gcloud", Args: []string{
			"gcloud", "sql", "instances", "delete", id, "--quiet",
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
		return ProviderCommand{Tool: "databricks", Args: []string{
			"databricks", "database", resource, "get", id,
		}}, nil
	case "destroy":
		return ProviderCommand{Tool: "databricks", Args: []string{
			"databricks", "database", resource, "delete", id,
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
		if arg == "instances" {
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
		return ProviderCommand{Tool: "aws", Args: []string{
			"aws", "rds", "describe-db-instances",
			"--db-instance-identifier", id,
		}}, "managed_provider", nil
	case ProviderGCPCloudSQL:
		return ProviderCommand{Tool: "gcloud", Args: []string{
			"gcloud", "sql", "instances", "describe", id,
			"--format=json(settings.backupConfiguration)",
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
		return ProviderCommand{Tool: "aws", Args: []string{
			"aws", "rds", "describe-db-snapshots",
			"--db-instance-identifier", id,
		}}, nil
	case ProviderGCPCloudSQL:
		return ProviderCommand{Tool: "gcloud", Args: []string{
			"gcloud", "sql", "backups", "list", "--instance", id,
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

func readiness(ctx context.Context, provider, label, cli string) ProviderReadiness {
	path, err := exec.LookPath(cli)
	out := ProviderReadiness{Provider: provider, Label: label, CLI: cli}
	if err != nil {
		out.Detail = "CLI not found on PATH"
		return out
	}
	out.Found = true
	out.Detail = path
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, cli, "--version").CombinedOutput()
	if err != nil {
		out.Version = strings.TrimSpace(string(version))
		return out
	}
	out.Version = strings.TrimSpace(string(version))
	return out
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
