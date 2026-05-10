package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/llm"
)

func agentDBBlueprintsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := st.Blueprints(r.Context())
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"blueprints": rows})
	}
}

func agentDBCreateBlueprintHandler(
	st *agentdb.Store,
	gen agentdb.BlueprintGenerator,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		blueprint, err := st.CreateBlueprint(
			r.Context(),
			agentdb.BlueprintDraftRequest{
				BlueprintID: str(m, "blueprint_id"),
				Name:        str(m, "name"),
				Intent:      str(m, "intent"),
				Provider:    str(m, "provider"),
				CreatedBy:   str(m, "created_by"),
			},
			gen,
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, blueprint)
	}
}

func agentDBApproveBlueprintHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		blueprint, err := st.ApproveBlueprint(
			r.Context(),
			r.PathValue("blueprint_id"),
			str(m, "approved_by"),
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, blueprint)
	}
}

func agentDBProvisionBlueprintHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		dep, err := st.ProvisionFromBlueprint(
			r.Context(),
			r.PathValue("blueprint_id"),
			agentdb.BlueprintProvisionRequest{
				DeploymentID:   str(m, "deployment_id"),
				TenantID:       str(m, "tenant_id"),
				AgentID:        str(m, "agent_id"),
				RunID:          str(m, "run_id"),
				DatabaseName:   str(m, "database_name"),
				LeaseSeconds:   integer(m, "lease_seconds"),
				BudgetUSD:      float(m, "budget_usd"),
				Metadata:       obj(m, "metadata"),
				ProviderParams: obj(m, "provider_params"),
			},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, dep)
	}
}

func newAgentDBBlueprintGenerator(mgr *llm.Manager) agentdb.BlueprintGenerator {
	if mgr == nil || mgr.General == nil || !mgr.General.IsEnabled() {
		return nil
	}
	return llmBlueprintGenerator{client: mgr.General}
}

type llmBlueprintGenerator struct {
	client *llm.Client
}

func (g llmBlueprintGenerator) GenerateBlueprint(
	ctx context.Context,
	req agentdb.BlueprintDraftRequest,
) (agentdb.BlueprintGeneration, error) {
	system := `You translate database deployment intent into strict JSON for pg_sage.
Return only one JSON object matching these fields:
provider, provisioning_level, region, instance_class, database_version,
lakebase_mode, storage_gb, backup_retention_days, pitr, multi_az,
private_network, public_ip, extensions, budget_usd, tags.
For aws_rds and gcp_cloudsql, region is required: copy the exact valid
provider region from the user intent, such as us-east-2 or us-central1.
If no valid cloud region is present, return region as an empty string so
pg_sage can ask for a correction instead of inventing a default.
Do not include credentials, secrets, tokens, tfvars, or Terraform.`
	user := fmt.Sprintf("Preferred provider: %s\nIntent:\n%s", req.Provider, req.Intent)
	raw, _, err := g.client.Chat(ctx, system, user, 4096)
	if err != nil {
		return agentdb.BlueprintGeneration{}, fmt.Errorf(
			"%w: %v", agentdb.ErrBlueprintLLMRequired, err)
	}
	var spec agentdb.BlueprintSpec
	if err := llm.ParseJSON(raw, llm.JSONObject, &spec); err != nil {
		return agentdb.BlueprintGeneration{}, fmt.Errorf(
			"%w: parse response: %v", agentdb.ErrBlueprintLLMRequired, err)
	}
	if spec.Provider == "" {
		spec.Provider = req.Provider
	}
	if err := validateLLMBlueprintSpec(spec); err != nil {
		return agentdb.BlueprintGeneration{}, err
	}
	spec = agentdb.NormalizeBlueprintSpec(spec, req.Intent)
	files, err := agentdb.RenderTerraformFromBlueprint(spec)
	if err != nil {
		if errors.Is(err, agentdb.ErrInvalid) {
			return agentdb.BlueprintGeneration{}, fmt.Errorf(
				"%w: invalid blueprint spec", agentdb.ErrBlueprintLLMRequired)
		}
		return agentdb.BlueprintGeneration{}, err
	}
	return agentdb.BlueprintGeneration{
		Spec:           spec,
		Files:          files,
		RawResponse:    raw,
		LLMUsed:        true,
		PolicyFindings: agentdb.BlueprintPolicyFindings(spec, req.Policy),
	}, nil
}

func validateLLMBlueprintSpec(spec agentdb.BlueprintSpec) error {
	provider := strings.ToLower(strings.TrimSpace(spec.Provider))
	switch provider {
	case "aws", "rds", "aws-rds":
		provider = agentdb.ProviderAWSRDS
	case "gcp", "cloudsql", "cloud-sql", "gcp-cloudsql":
		provider = agentdb.ProviderGCPCloudSQL
	}
	if (provider == agentdb.ProviderAWSRDS || provider == agentdb.ProviderGCPCloudSQL) &&
		strings.TrimSpace(spec.Region) == "" {
		return fmt.Errorf("%w: region is required", agentdb.ErrBlueprintLLMRequired)
	}
	return nil
}
