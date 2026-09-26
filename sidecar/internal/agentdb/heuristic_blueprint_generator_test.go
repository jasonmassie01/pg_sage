package agentdb

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// HeuristicBlueprintGenerator is a deterministic, regex-based generator used
// only by tests (G8-D01). Production fails closed without an LLM
// (ErrBlueprintLLMRequired), so it never ships in the binary. It lives in a
// _test.go file because in-package tests cannot import a helper package that
// itself imports agentdb.
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

func inferProvider(provider, intent string) string {
	if provider != "" {
		return normalizeProvider(provider)
	}
	lower := strings.ToLower(intent)
	switch {
	case strings.Contains(lower, "supabase"):
		return ProviderSupabase
	case strings.Contains(lower, "neon"):
		return ProviderNeon
	case strings.Contains(lower, "cloud sql") || strings.Contains(lower, "gcp"):
		return ProviderGCPCloudSQL
	case strings.Contains(lower, "lakebase") || strings.Contains(lower, "databricks"):
		return ProviderDatabricksLakebase
	default:
		return ProviderAWSRDS
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
