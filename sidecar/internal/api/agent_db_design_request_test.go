package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

// D4 follow-up: blueprint and Terraform-template provisioning of a cloud
// plan consumes an approved, single-use request attributed to its approver.

func seedDesignAPIFixtures(t *testing.T, f *d4Fixture, prefix string) {
	t.Helper()
	clean := func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, "DELETE FROM sage.agent_db_blueprints WHERE blueprint_id=$1",
			prefix+"_bp")
		_, _ = f.pool.Exec(c, `DELETE FROM sage.agent_db_terraform_templates
			WHERE template_id IN ($1, $2)`, prefix+"_bp_tf", prefix+"_tpl")
	}
	clean()
	t.Cleanup(clean)
	if _, err := f.st.CreateBlueprint(f.ctx, agentdb.BlueprintDraftRequest{
		BlueprintID: prefix + "_bp", Name: prefix, CreatedBy: "unit",
	}, apiStaticBlueprintGenerator{spec: agentdb.BlueprintSpec{
		Provider: agentdb.ProviderAWSRDS, ProvisioningLevel: agentdb.LevelInstance,
		Region: "us-east-2", InstanceClass: "db.t4g.micro", StorageGB: 20,
		BackupRetentionDays: 1, PrivateNetwork: true,
	}}); err != nil {
		t.Fatalf("CreateBlueprint: %v", err)
	}
	if _, err := f.st.ApproveBlueprint(f.ctx, prefix+"_bp", "reviewer@x.test"); err != nil {
		t.Fatalf("ApproveBlueprint: %v", err)
	}
	if _, err := f.st.CreateTerraformTemplate(f.ctx, agentdb.TerraformTemplateRequest{
		TemplateID: prefix + "_tpl", Name: prefix, SourceKind: "upload", CreatedBy: "unit",
		Files: []agentdb.TerraformFile{{Path: "main.tf", Body: `provider "aws" {}
resource "aws_db_instance" "agentdb" { engine = "postgres" }`}},
	}); err != nil {
		t.Fatalf("CreateTerraformTemplate: %v", err)
	}
	if _, err := f.st.ApproveTerraformTemplate(f.ctx, prefix+"_tpl",
		"reviewer@x.test"); err != nil {
		t.Fatalf("ApproveTerraformTemplate: %v", err)
	}
}

func designPaths(prefix string) map[string]string {
	return map[string]string{
		"blueprint": "/api/v1/agent-dbs/blueprints/" + prefix + "_bp/provision",
		"template":  "/api/v1/agent-dbs/terraform-templates/" + prefix + "_tpl/provision",
	}
}

func designBody(requestID, depID string) string {
	return `{"request_id":"` + requestID + `","deployment_id":"` + depID + `",` +
		`"tenant_id":"` + d4APITenant + `","agent_id":"agent_d4_api",` +
		`"provider":"aws_rds","provisioning_level":"instance",` +
		`"provider_params":{"region":"us-east-2","db_instance_class":"db.t4g.micro"}}`
}

func TestDesignProvisionWithoutRequestRejected(t *testing.T) {
	f := newD4Fixture(t)
	seedDesignAPIFixtures(t, f, "d4_api_design_noreq")
	for kind, path := range designPaths("d4_api_design_noreq") {
		rr := f.do(testOperatorUser(), http.MethodPost, path,
			designBody("", "dep_d4_api_design_noreq_"+kind))
		if rr.Code != http.StatusConflict ||
			!strings.Contains(rr.Body.String(), "approved request required") {
			t.Fatalf("%s without request: %d %s", kind, rr.Code, rr.Body.String())
		}
	}
	if n := f.deploymentCount(t, "deployment_id LIKE 'dep_d4_api_design_noreq_%'"); n != 0 {
		t.Fatalf("rejected design provisions left %d rows", n)
	}
}

func TestDesignProvisionConsumesRequestOnce(t *testing.T) {
	f := newD4Fixture(t)
	seedDesignAPIFixtures(t, f, "d4_api_design_once")
	for kind, path := range designPaths("d4_api_design_once") {
		requestID := "req_d4_api_design_once_" + kind
		f.createRequest(t, requestID, "agent_d4_api", 0)
		approve := f.do(testAdminUser(), http.MethodPost,
			"/api/v1/agent-dbs/requests/"+requestID+"/approve", `{}`)
		if approve.Code != http.StatusOK {
			t.Fatalf("approve: %d %s", approve.Code, approve.Body.String())
		}
		rr := f.do(testOperatorUser(), http.MethodPost, path,
			designBody(requestID, "dep_d4_api_design_once_"+kind))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s provision: %d %s", kind, rr.Code, rr.Body.String())
		}
		var dep agentdb.Deployment
		if err := json.Unmarshal(rr.Body.Bytes(), &dep); err != nil ||
			dep.Metadata["request_id"] != requestID {
			t.Fatalf("%s deployment not linked: %s", kind, rr.Body.String())
		}
		got, err := f.st.GetRequest(f.ctx, requestID)
		if err != nil || got.DecidedBy != testAdminUser().Email ||
			got.ConsumedBy != testOperatorUser().Email {
			t.Fatalf("%s attribution = %+v %v", kind, got, err)
		}
		again := f.do(testOperatorUser(), http.MethodPost, path,
			designBody(requestID, "dep_d4_api_design_again_"+kind))
		if again.Code != http.StatusConflict {
			t.Fatalf("%s reuse: %d %s", kind, again.Code, again.Body.String())
		}
	}
}

func TestDesignProvisionRequiresUser(t *testing.T) {
	f := newD4Fixture(t)
	seedDesignAPIFixtures(t, f, "d4_api_design_anon")
	f.createRequest(t, "req_d4_api_design_anon", "agent_d4_api", 25)
	handler := agentDBSubrouterWithRegistry(f.st, agentdb.DefaultRunnerRegistry(), nil)
	for kind, path := range designPaths("d4_api_design_anon") {
		rr := doRequest(handler, http.MethodPost, path,
			designBody("req_d4_api_design_anon", "dep_d4_api_design_anon_"+kind))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s anonymous: %d %s", kind, rr.Code, rr.Body.String())
		}
	}
}
