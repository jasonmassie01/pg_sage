package agentdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type HeuristicBlueprintGenerator struct{}

func NewHeuristicBlueprintGenerator() HeuristicBlueprintGenerator {
	return HeuristicBlueprintGenerator{}
}

func (HeuristicBlueprintGenerator) GenerateBlueprint(
	_ context.Context,
	req BlueprintDraftRequest,
) (BlueprintGeneration, error) {
	intent := strings.TrimSpace(req.Intent)
	if intent == "" {
		return BlueprintGeneration{}, ErrInvalid
	}
	spec := BlueprintSpec{
		Provider:            inferProvider(req.Provider, intent),
		ProvisioningLevel:   LevelInstance,
		Region:              inferRegion(intent),
		StorageGB:           inferFirstInt(intent, `(?i)(\d+)\s*(gb|gib)`),
		BackupRetentionDays: inferBackupDays(intent),
		PITR:                hasAny(intent, "pitr", "point in time", "point-in-time"),
		MultiAZ:             hasAny(intent, "multi-az", "multi az", "high availability", " ha "),
		PrivateNetwork:      hasAny(intent, "private", "vpc", "private network", "privatelink"),
		PublicIP:            hasAny(intent, "public ip", "public ipv4", "publicly accessible"),
		Extensions:          inferExtensions(intent),
		BudgetUSD:           inferBudget(intent),
		Tags:                map[string]string{"managed_by": "pg_sage"},
	}
	spec = NormalizeBlueprintSpec(spec, intent)
	files, err := RenderTerraformFromBlueprint(spec)
	if err != nil {
		return BlueprintGeneration{}, err
	}
	return BlueprintGeneration{
		Spec:           spec,
		Files:          files,
		PolicyFindings: BlueprintPolicyFindings(spec, req.Policy),
	}, nil
}

func RenderTerraformFromBlueprint(spec BlueprintSpec) ([]TerraformFile, error) {
	spec = NormalizeBlueprintSpec(spec, "")
	if spec.ProvisioningLevel != LevelInstance || !validProvider(spec.Provider) {
		return nil, ErrInvalid
	}
	switch spec.Provider {
	case ProviderAWSRDS:
		return []TerraformFile{{Path: "main.tf", Body: renderAWSRDS(spec)}}, nil
	case ProviderGCPCloudSQL:
		return []TerraformFile{{Path: "main.tf", Body: renderCloudSQL(spec)}}, nil
	case ProviderDatabricksLakebase:
		return []TerraformFile{{Path: "main.tf", Body: renderLakebase(spec)}}, nil
	default:
		return nil, ErrInvalid
	}
}

func NormalizeBlueprintSpec(spec BlueprintSpec, intent string) BlueprintSpec {
	spec.Provider = normalizeProvider(spec.Provider)
	spec.ProvisioningLevel = normalizeProvisioningLevel(spec.ProvisioningLevel)
	if spec.ProvisioningLevel == LevelSchema && spec.Provider != ProviderLocalPostgres {
		spec.ProvisioningLevel = LevelInstance
	}
	if spec.Tags == nil {
		spec.Tags = map[string]string{"managed_by": "pg_sage"}
	}
	fillProviderDefaults(&spec, intent)
	return spec
}

func BlueprintPolicyFindings(spec BlueprintSpec, policy BlueprintPolicy) []string {
	if policy.MinimumBackupRetentionDays == 0 {
		policy.MinimumBackupRetentionDays = 1
	}
	findings := []string{}
	if !validProvider(spec.Provider) || normalizeProvider(spec.Provider) == ProviderLocalPostgres {
		findings = append(findings, "cloud provider is required for blueprint terraform generation")
	}
	if normalizeProvisioningLevel(spec.ProvisioningLevel) != LevelInstance {
		findings = append(findings, "cloud blueprints must use instance provisioning level")
	}
	if spec.PublicIP && !policy.AllowPublicIP {
		findings = append(findings, "public ip is denied by blueprint policy")
	}
	if policy.RequirePrivateNetworking && !spec.PrivateNetwork {
		findings = append(findings, "private network is required by blueprint policy")
	}
	if spec.BackupRetentionDays < policy.MinimumBackupRetentionDays {
		findings = append(findings, "backup retention is below blueprint policy minimum")
	}
	return findings
}

func (s *Store) CreateBlueprint(
	ctx context.Context,
	req BlueprintDraftRequest,
	gen BlueprintGenerator,
) (Blueprint, error) {
	if err := s.Ensure(ctx); err != nil {
		return Blueprint{}, err
	}
	req.BlueprintID = sanitizeSchemaName(req.BlueprintID)
	if req.BlueprintID == "" {
		req.BlueprintID = "bp_" + idFrom(req.Name, req.Intent)
	}
	if gen == nil {
		return Blueprint{}, ErrBlueprintLLMRequired
	}
	generated, err := gen.GenerateBlueprint(ctx, req)
	if err != nil {
		return Blueprint{}, err
	}
	files := generated.Files
	if len(files) == 0 {
		files, err = RenderTerraformFromBlueprint(generated.Spec)
		if err != nil {
			return Blueprint{}, err
		}
	}
	templateID := req.BlueprintID + "_tf"
	template, err := s.CreateTerraformTemplate(ctx, TerraformTemplateRequest{
		TemplateID: templateID,
		Name:       firstNonEmpty(req.Name, req.BlueprintID) + " Terraform",
		SourceKind: "blueprint",
		Files:      files,
		CreatedBy:  req.CreatedBy,
	})
	if err != nil {
		return Blueprint{}, err
	}
	findings := append([]string{}, generated.PolicyFindings...)
	findings = append(findings, template.PolicyFindings...)
	status := "generated"
	if len(findings) > 0 {
		status = "rejected"
	}
	blueprint := Blueprint{
		BlueprintID:    req.BlueprintID,
		Name:           firstNonEmpty(req.Name, req.BlueprintID),
		Status:         status,
		Intent:         req.Intent,
		Provider:       generated.Spec.Provider,
		TemplateID:     templateID,
		Spec:           generated.Spec,
		PolicyFindings: findings,
		LLMUsed:        generated.LLMUsed,
		RawResponse:    generated.RawResponse,
		CreatedBy:      req.CreatedBy,
	}
	err = scanBlueprint(s.pool.QueryRow(ctx, `/* pg_sage */ 
		INSERT INTO sage.agent_db_blueprints (
			blueprint_id, name, status, intent, provider, template_id,
			blueprint_json, policy_findings, llm_used, raw_response, created_by
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9, $10, $11)
		ON CONFLICT (blueprint_id) DO UPDATE
		SET name=EXCLUDED.name,
			status=EXCLUDED.status,
			intent=EXCLUDED.intent,
			provider=EXCLUDED.provider,
			template_id=EXCLUDED.template_id,
			blueprint_json=EXCLUDED.blueprint_json,
			policy_findings=EXCLUDED.policy_findings,
			llm_used=EXCLUDED.llm_used,
			raw_response=EXCLUDED.raw_response,
			updated_at=now()
		RETURNING blueprint_id, name, status, intent, provider, template_id,
			blueprint_json, policy_findings, llm_used, raw_response, created_by,
			approved_by, created_at, updated_at`,
		blueprint.BlueprintID,
		blueprint.Name,
		blueprint.Status,
		blueprint.Intent,
		blueprint.Provider,
		blueprint.TemplateID,
		jsonAny(blueprint.Spec),
		jsonAny(blueprint.PolicyFindings),
		blueprint.LLMUsed,
		blueprint.RawResponse,
		blueprint.CreatedBy,
	), &blueprint)
	return blueprint, err
}

func (s *Store) Blueprints(ctx context.Context) ([]Blueprint, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ 
		SELECT blueprint_id, name, status, intent, provider, template_id,
			blueprint_json, policy_findings, llm_used, raw_response, created_by,
			approved_by, created_at, updated_at
		FROM sage.agent_db_blueprints
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Blueprint{}
	for rows.Next() {
		var blueprint Blueprint
		if err := scanBlueprint(rows, &blueprint); err != nil {
			return nil, err
		}
		out = append(out, blueprint)
	}
	return out, rows.Err()
}

func scanBlueprint(row scanner, blueprint *Blueprint) error {
	var specJSON []byte
	var findingsJSON []byte
	err := row.Scan(
		&blueprint.BlueprintID,
		&blueprint.Name,
		&blueprint.Status,
		&blueprint.Intent,
		&blueprint.Provider,
		&blueprint.TemplateID,
		&specJSON,
		&findingsJSON,
		&blueprint.LLMUsed,
		&blueprint.RawResponse,
		&blueprint.CreatedBy,
		&blueprint.ApprovedBy,
		&blueprint.CreatedAt,
		&blueprint.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	_ = json.Unmarshal(specJSON, &blueprint.Spec)
	_ = json.Unmarshal(findingsJSON, &blueprint.PolicyFindings)
	return nil
}

func inferProvider(provider, intent string) string {
	if provider != "" {
		return normalizeProvider(provider)
	}
	lower := strings.ToLower(intent)
	switch {
	case strings.Contains(lower, "cloud sql") || strings.Contains(lower, "gcp"):
		return ProviderGCPCloudSQL
	case strings.Contains(lower, "lakebase") || strings.Contains(lower, "databricks"):
		return ProviderDatabricksLakebase
	default:
		return ProviderAWSRDS
	}
}

func fillProviderDefaults(spec *BlueprintSpec, intent string) {
	switch spec.Provider {
	case ProviderAWSRDS:
		spec.InstanceClass = firstNonEmpty(spec.InstanceClass, inferClass(intent, `db\.[a-z0-9.]+`), "db.t4g.micro")
		spec.DatabaseVersion = firstNonEmpty(spec.DatabaseVersion, "16")
	case ProviderGCPCloudSQL:
		spec.InstanceClass = firstNonEmpty(spec.InstanceClass, inferClass(intent, `db-[a-z0-9-]+`), "db-custom-1-3840")
		spec.DatabaseVersion = firstNonEmpty(spec.DatabaseVersion, "POSTGRES_16")
	case ProviderDatabricksLakebase:
		spec.LakebaseMode = "autoscaling_branch"
		if hasAny(intent, "instance", "provisioned") {
			spec.LakebaseMode = "provisioned_instance"
		}
		spec.DatabaseVersion = firstNonEmpty(spec.DatabaseVersion, "lakebase")
	}
	if spec.Region == "" {
		spec.Region = defaultRegion(spec.Provider)
	}
	if spec.StorageGB == 0 {
		spec.StorageGB = 20
	}
	if spec.BackupRetentionDays == 0 && (spec.PITR || spec.MultiAZ) {
		spec.BackupRetentionDays = 7
	}
}

func inferRegion(intent string) string {
	re := regexp.MustCompile(`(?i)\b([a-z]{2}-[a-z]+-\d)\b`)
	match := re.FindStringSubmatch(intent)
	if len(match) > 1 {
		return strings.ToLower(match[1])
	}
	return ""
}

func inferFirstInt(intent, pattern string) int {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(intent)
	if len(match) < 2 {
		return 0
	}
	value, _ := strconv.Atoi(match[1])
	return value
}

func inferBackupDays(intent string) int {
	return inferFirstInt(intent, `(?i)(\d+)[-\s]*day(?:s)?\s+backup`)
}

func inferBudget(intent string) float64 {
	re := regexp.MustCompile(`(?i)\$([0-9]+(?:\.[0-9]+)?)`)
	match := re.FindStringSubmatch(intent)
	if len(match) < 2 {
		return 0
	}
	value, _ := strconv.ParseFloat(match[1], 64)
	return value
}

func inferClass(intent, pattern string) string {
	re := regexp.MustCompile(`(?i)\b` + pattern + `\b`)
	return strings.ToLower(re.FindString(intent))
}

func inferExtensions(intent string) []string {
	lower := strings.ToLower(intent)
	known := []string{"pgvector", "postgis", "pg_stat_statements", "uuid-ossp"}
	out := []string{}
	for _, extension := range known {
		if strings.Contains(lower, strings.ReplaceAll(extension, "_", " ")) ||
			strings.Contains(lower, extension) {
			out = append(out, extension)
		}
	}
	return out
}

func hasAny(intent string, needles ...string) bool {
	lower := " " + strings.ToLower(intent) + " "
	for _, needle := range needles {
		if strings.Contains(lower, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

func defaultRegion(provider string) string {
	switch provider {
	case ProviderGCPCloudSQL:
		return "us-central1"
	case ProviderDatabricksLakebase:
		return "default"
	default:
		return "us-east-1"
	}
}

func renderAWSRDS(spec BlueprintSpec) string {
	return fmt.Sprintf(`terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = %q
}

variable "master_username" {
  type = string
}

variable "master_password" {
  type      = string
  sensitive = true
}

locals {
  pg_sage_extensions = %s
}

resource "aws_db_instance" "agentdb" {
  identifier              = "pg-sage-agentdb"
  engine                  = "postgres"
  engine_version          = %q
  instance_class          = %q
  allocated_storage       = %d
  storage_encrypted       = true
  multi_az                = %t
  publicly_accessible     = %t
  backup_retention_period = %d
  deletion_protection     = true
  skip_final_snapshot     = false
  username                = var.master_username
  password                = var.master_password
}
`, spec.Region, terraformStringList(spec.Extensions), spec.DatabaseVersion,
		spec.InstanceClass, spec.StorageGB, spec.MultiAZ, spec.PublicIP,
		spec.BackupRetentionDays)
}

func renderCloudSQL(spec BlueprintSpec) string {
	return fmt.Sprintf(`terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.0"
    }
  }
}

provider "google" {
  region = %q
}

locals {
  pg_sage_extensions = %s
}

resource "google_sql_database_instance" "agentdb" {
  name             = "pg-sage-agentdb"
  database_version = %q
  region           = %q
  deletion_protection = true

  settings {
    tier              = %q
    edition           = "ENTERPRISE"
    disk_size         = %d
    availability_type = %q
    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = %t
      transaction_log_retention_days = %d
    }
    ip_configuration {
      ipv4_enabled = %t
      require_ssl  = true
    }
  }
}
`, spec.Region, terraformStringList(spec.Extensions), spec.DatabaseVersion,
		spec.Region, spec.InstanceClass, spec.StorageGB,
		availabilityType(spec.MultiAZ), spec.PITR, spec.BackupRetentionDays,
		spec.PublicIP)
}

func renderLakebase(spec BlueprintSpec) string {
	resource := "databricks_database_instance"
	if spec.LakebaseMode == "autoscaling_branch" {
		resource = "databricks_database_branch"
	}
	return fmt.Sprintf(`terraform {
  required_providers {
    databricks = {
      source  = "databricks/databricks"
      version = "~> 1.0"
    }
  }
}

locals {
  pg_sage_extensions = %s
}

resource %q "agentdb" {
  name = "pg-sage-agentdb"
}
`, terraformStringList(spec.Extensions), resource)
}

func terraformStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func availabilityType(multiAZ bool) string {
	if multiAZ {
		return "REGIONAL"
	}
	return "ZONAL"
}
