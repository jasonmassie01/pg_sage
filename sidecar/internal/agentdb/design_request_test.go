package agentdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// D4 follow-up: an approved blueprint or Terraform template is a reviewed
// design, not permission to spend. Planning a cloud deployment from one
// consumes an approved, single-use agent request, exactly like the direct
// request path.

const designTenant = "tenant_d4_design"

func seedDesignBlueprint(
	t *testing.T, st *Store, ctx context.Context, pool *pgxpool.Pool, id string,
) {
	t.Helper()
	clean := func() {
		c := context.Background()
		_, _ = pool.Exec(c, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1", id)
		_, _ = pool.Exec(c,
			"DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", id+"_tf")
	}
	clean()
	t.Cleanup(clean)
	if _, err := st.CreateBlueprint(ctx, BlueprintDraftRequest{BlueprintID: id,
		Name: id, CreatedBy: "unit"}, staticBlueprintGenerator{spec: BlueprintSpec{
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance, Region: "us-east-2",
		InstanceClass: "db.t4g.micro", StorageGB: 20, BackupRetentionDays: 1,
		PrivateNetwork: true,
	}}); err != nil {
		t.Fatalf("CreateBlueprint: %v", err)
	}
	if _, err := st.ApproveBlueprint(ctx, id, "design-reviewer@x.test"); err != nil {
		t.Fatalf("ApproveBlueprint: %v", err)
	}
}

func seedDesignTemplate(
	t *testing.T, st *Store, ctx context.Context, pool *pgxpool.Pool, id string,
) {
	t.Helper()
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.agent_db_terraform_templates WHERE template_id=$1", id)
	}
	clean()
	t.Cleanup(clean)
	if _, err := st.CreateTerraformTemplate(ctx, TerraformTemplateRequest{
		TemplateID: id, Name: id, CreatedBy: "unit",
		SourceKind: "upload",
		Files: []TerraformFile{{Path: "main.tf", Body: `provider "aws" {}
resource "aws_db_instance" "agentdb" { engine = "postgres" }`}},
	}); err != nil {
		t.Fatalf("CreateTerraformTemplate: %v", err)
	}
	if _, err := st.ApproveTerraformTemplate(ctx, id, "design-reviewer@x.test"); err != nil {
		t.Fatalf("ApproveTerraformTemplate: %v", err)
	}
}

// seedDesignRequest creates an aws_rds instance request that a human
// approves, so the approval is attributed to approver.
func seedDesignRequest(
	t *testing.T, st *Store, ctx context.Context, pool *pgxpool.Pool, id, approver string,
) {
	t.Helper()
	resetD4Request(t, ctx, pool, id)
	if _, err := st.CreateRequest(ctx, RequestCreate{RequestID: id, TenantID: designTenant,
		AgentID: "agent_design", IsolationType: LevelInstance, Provider: ProviderAWSRDS,
		BackupRequired: true}); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if approver == "" {
		return
	}
	if _, err := st.SetRequestDecision(ctx, id,
		DecisionRequest{Decision: "approved", ActorID: approver}); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

func blueprintReq(requestID, depID string) BlueprintProvisionRequest {
	return BlueprintProvisionRequest{RequestID: requestID, DeploymentID: depID,
		TenantID: designTenant, AgentID: "agent_design", ActorID: "op@x.test"}
}

func templateReq(requestID, depID string) TemplateProvisionRequest {
	return TemplateProvisionRequest{RequestID: requestID, DeploymentID: depID,
		TenantID: designTenant, AgentID: "agent_design", Provider: ProviderAWSRDS,
		ActorID: "op@x.test",
		ProviderParams: map[string]any{"region": "us-east-2",
			"db_instance_class": "db.t4g.micro"}}
}

type designProvisioner func(requestID, depID string) (Deployment, error)

func designProvisioners(
	t *testing.T, st *Store, ctx context.Context, pool *pgxpool.Pool, prefix string,
) map[string]designProvisioner {
	t.Helper()
	seedDesignBlueprint(t, st, ctx, pool, prefix+"_bp")
	seedDesignTemplate(t, st, ctx, pool, prefix+"_tpl")
	return map[string]designProvisioner{
		"blueprint": func(requestID, depID string) (Deployment, error) {
			return st.ProvisionFromBlueprint(ctx, prefix+"_bp", blueprintReq(requestID, depID))
		},
		"template": func(requestID, depID string) (Deployment, error) {
			return st.ProvisionFromTerraformTemplate(ctx, prefix+"_tpl",
				templateReq(requestID, depID))
		},
	}
}

func TestDesignProvisionRequiresApprovedRequest(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	for kind, provision := range designProvisioners(t, st, ctx, pool, "d4_design_req") {
		if _, err := provision("", "dep_d4_design_noreq_"+kind); !errors.Is(err,
			ErrApprovalRequired) {
			t.Fatalf("%s without request err = %v, want ErrApprovalRequired", kind, err)
		}
		pending := "req_d4_design_pending_" + kind
		seedDesignRequest(t, st, ctx, pool, pending, "")
		if _, err := provision(pending, "dep_d4_design_pending_"+kind); !errors.Is(err,
			ErrInvalid) {
			t.Fatalf("%s with unapproved request err = %v, want ErrInvalid", kind, err)
		}
		if _, err := provision("req_d4_design_missing", "dep_d4_x"); !errors.Is(err,
			ErrNotFound) {
			t.Fatalf("%s with unknown request err = %v, want ErrNotFound", kind, err)
		}
	}
}

func TestDesignProvisionConsumesRequestOnceAndRecordsApprovers(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	for kind, provision := range designProvisioners(t, st, ctx, pool, "d4_design_once") {
		requestID := "req_d4_design_once_" + kind
		seedDesignRequest(t, st, ctx, pool, requestID, "lead@x.test")
		dep, err := provision(requestID, "dep_d4_design_once_"+kind)
		if err != nil {
			t.Fatalf("%s provision: %v", kind, err)
		}
		if dep.Metadata["request_id"] != requestID || dep.TenantID != designTenant {
			t.Fatalf("%s deployment not linked: %#v", kind, dep.Metadata)
		}
		got, err := st.GetRequest(ctx, requestID)
		if err != nil {
			t.Fatal(err)
		}
		if got.DecidedBy != "lead@x.test" || got.ConsumedBy != "op@x.test" ||
			got.ConsumedDeploymentID != dep.DeploymentID {
			t.Fatalf("%s request attribution = %+v", kind, got)
		}
		var detail string
		if err := pool.QueryRow(ctx, `SELECT detail::text FROM sage.agent_db_audit
			WHERE deployment_id=$1 AND event='request_consumed'`,
			dep.DeploymentID).Scan(&detail); err != nil {
			t.Fatalf("%s consumption audit: %v", kind, err)
		}
		for _, want := range []string{"lead@x.test", "op@x.test", "design-reviewer@x.test"} {
			if !strings.Contains(detail, want) {
				t.Fatalf("%s audit %s missing %s", kind, detail, want)
			}
		}
		for _, depID := range []string{dep.DeploymentID, "dep_d4_design_again_" + kind} {
			if _, err := provision(requestID, depID); !errors.Is(err, ErrConflict) {
				t.Fatalf("%s reuse into %s err = %v, want ErrConflict", kind, depID, err)
			}
		}
	}
}

func TestDesignProvisionRejectsRequestForOtherOwner(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	seedDesignBlueprint(t, st, ctx, pool, "d4_design_owner_bp")
	seedDesignTemplate(t, st, ctx, pool, "d4_design_owner_tpl")
	seedDesignRequest(t, st, ctx, pool, "req_d4_design_owner", "lead@x.test")
	bp := blueprintReq("req_d4_design_owner", "dep_d4_design_owner")
	bp.AgentID = "agent_other"
	if _, err := st.ProvisionFromBlueprint(ctx, "d4_design_owner_bp", bp); !errors.Is(err,
		ErrConflict) {
		t.Fatalf("blueprint for other agent err = %v, want ErrConflict", err)
	}
	tpl := templateReq("req_d4_design_owner", "dep_d4_design_owner")
	tpl.TenantID = "tenant_other"
	if _, err := st.ProvisionFromTerraformTemplate(ctx, "d4_design_owner_tpl",
		tpl); !errors.Is(err, ErrConflict) {
		t.Fatalf("template for other tenant err = %v, want ErrConflict", err)
	}
	tpl = templateReq("req_d4_design_owner", "dep_d4_design_owner")
	tpl.Provider = ProviderGCPCloudSQL
	if _, err := st.ProvisionFromTerraformTemplate(ctx, "d4_design_owner_tpl",
		tpl); !errors.Is(err, ErrConflict) {
		t.Fatalf("template for other provider err = %v, want ErrConflict", err)
	}
	got, err := st.GetRequest(ctx, "req_d4_design_owner")
	if err != nil || got.ConsumedDeploymentID != "" {
		t.Fatalf("mismatched design provision consumed the request: %+v %v", got, err)
	}
}

func TestDesignProvisionFailureReleasesClaim(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	taken := "dep_d4_design_taken"
	cleanupDeployment(t, ctx, pool, taken)
	if _, err := st.Register(ctx, RegisterRequest{DeploymentID: taken,
		TenantID: "tenant_someone_else", AgentID: "agent_else",
		IsolationType: LevelSchema}); err != nil {
		t.Fatalf("seed taken deployment: %v", err)
	}
	for kind, provision := range designProvisioners(t, st, ctx, pool, "d4_design_fail") {
		requestID := "req_d4_design_fail_" + kind
		seedDesignRequest(t, st, ctx, pool, requestID, "lead@x.test")
		if _, err := provision(requestID, taken); err == nil {
			t.Fatalf("%s provision into another owner's deployment succeeded", kind)
		}
		got, err := st.GetRequest(ctx, requestID)
		if err != nil || got.ConsumedDeploymentID != "" || got.ConsumedAt != nil {
			t.Fatalf("%s failed provision kept the claim: %+v %v", kind, got, err)
		}
		if _, err := provision(requestID, "dep_d4_design_fail_ok_"+kind); err != nil {
			t.Fatalf("%s retry after failure: %v", kind, err)
		}
	}
}

func TestDesignProvisionConcurrentExactlyOne(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	for kind, provision := range designProvisioners(t, st, ctx, pool, "d4_design_race") {
		for _, shared := range []bool{true, false} {
			requestID := fmt.Sprintf("req_d4_design_race_%s_%v", kind, shared)
			seedDesignRequest(t, st, ctx, pool, requestID, "lead@x.test")
			wins, losses := raceDesign(provision, requestID, shared)
			for _, err := range losses {
				if !errors.Is(err, ErrConflict) {
					t.Errorf("%s shared=%v loser err = %v, want ErrConflict",
						kind, shared, err)
				}
			}
			if wins != 1 {
				t.Fatalf("%s shared=%v winners = %d, want 1", kind, shared, wins)
			}
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.agent_db_deployments
				WHERE metadata->>'request_id'=$1`, requestID).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s shared=%v deployments = %d err=%v", kind, shared, n, err)
			}
		}
	}
}

func raceDesign(provision designProvisioner, requestID string, shared bool) (int, []error) {
	const racers = 6
	var wg sync.WaitGroup
	results := make(chan error, racers)
	for i := 0; i < racers; i++ {
		depID := requestID + "_dep"
		if !shared {
			depID = fmt.Sprintf("%s_dep_%d", requestID, i)
		}
		wg.Add(1)
		go func(depID string) {
			defer wg.Done()
			_, err := provision(requestID, depID)
			results <- err
		}(depID)
	}
	wg.Wait()
	close(results)
	wins, losses := 0, []error{}
	for err := range results {
		if err == nil {
			wins++
		} else {
			losses = append(losses, err)
		}
	}
	return wins, losses
}

// approvedCloudRequest creates a cloud instance request that request policy
// approves at creation (positive budget), for fixtures that plan from a
// blueprint or template. Any earlier row with the same id is replaced.
func approvedCloudRequest(
	ctx context.Context, st *Store, id, tenant, agent, provider string, budget float64,
) (string, error) {
	if _, err := st.pool.Exec(ctx,
		"DELETE FROM sage.agent_db_requests WHERE request_id=$1", id); err != nil {
		return "", err
	}
	req, err := st.CreateRequest(ctx, RequestCreate{RequestID: id, TenantID: tenant,
		AgentID: agent, IsolationType: LevelInstance, Provider: provider,
		BudgetUSD: budget, BackupRequired: true})
	if err != nil {
		return "", err
	}
	if req.Status != "approved" {
		return "", fmt.Errorf("fixture request %s is %s, want approved", id, req.Status)
	}
	return id, nil
}

func mustApprovedCloudRequest(
	t *testing.T, ctx context.Context, st *Store, id, tenant, agent, provider string,
) string {
	t.Helper()
	got, err := approvedCloudRequest(ctx, st, id, tenant, agent, provider, 20)
	if err != nil {
		t.Fatalf("approved request fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(),
			"DELETE FROM sage.agent_db_requests WHERE request_id=$1", id)
	})
	return got
}
