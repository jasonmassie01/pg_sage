package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestPreflightSurfaceAgentDBRoleAndLiveBoundary(t *testing.T) {
	pool := surfacePool(t)
	cfg := config.DefaultConfig()
	f := surfaceRouter(t, pool, cfg, nil)
	f.login(t, "viewer")
	status, body := f.request(t, "POST", "/api/v1/agent-dbs",
		`{"deployment_id":"surface_viewer","tenant_id":"fixture","agent_id":"fixture",`+
			`"provider":"local_postgres","provisioning_level":"schema","schema_name":"surface_viewer"}`)
	if status != 403 {
		t.Fatalf("viewer registration=%d %s", status, body)
	}
	if n := surfaceCount(t, pool, `SELECT count(*) FROM pg_namespace WHERE nspname='surface_viewer'`); n != 0 {
		t.Fatalf("viewer created %d schemas", n)
	}
	// New authenticated browser, same production router and database, different real login.
	t.Run("operator", func(t *testing.T) {
		f.login(t, "operator")
		status, body := f.request(t, "POST", "/api/v1/agent-dbs/provider-configs/aws_rds", `{"enabled":true}`)
		if status != 403 {
			t.Errorf("operator provider config=%d %s", status, body)
		}
		status, body = f.request(t, "POST", "/api/v1/agent-dbs/fixture/provision/authorize-live",
			`{"operation":"create","idempotency_key":"fixture"}`)
		if status != 403 {
			t.Errorf("operator authorize-live=%d %s", status, body)
		}
		status, body = f.request(t, "POST", "/api/v1/agent-dbs",
			`{"deployment_id":"surface_live_plan","tenant_id":"fixture","agent_id":"fixture",`+
				`"provider":"aws_rds","provisioning_level":"instance","budget_usd":100}`)
		if status != 200 {
			t.Fatalf("persist cloud plan only=%d %s", status, body)
		}
		status, body = f.request(t, "POST", "/api/v1/agent-dbs/surface_live_plan/provision/execute",
			`{"mode":"live","approved":true,"actor_id":"admin","estimated_cost_usd":1}`)
		if status != 400 && status != 403 && status != 409 {
			t.Errorf("spoofed live authority accepted=%d %s", status, body)
		}
		if n := surfaceCount(t, pool, `SELECT count(*) FROM sage.agent_db_deployments
   WHERE deployment_id='surface_live_plan' AND provisioning_status='planned'
   AND COALESCE(provider_resource_id,'')=''`); n != 1 {
			t.Errorf("rejected live request changed durable plan or resource ownership")
		}
		t.Logf("existing cloud plan: caller approval claims rejected: %d %s; no resource ID persisted", status, body)
	})
}

func TestPreflightSurfaceAgentDBSchemaLifecycle(t *testing.T) {
	pool := surfacePool(t)
	// G8-B17: local_postgres DDL requires the explicit opt-in.
	t.Setenv("PG_SAGE_AGENTDB_LOCAL_PROVISIONING", "1")
	f := surfaceRouter(t, pool, config.DefaultConfig(), nil)
	f.login(t, "operator")
	root := "/api/v1/agent-dbs/surface_lifecycle"
	status, body := f.request(t, "POST", "/api/v1/agent-dbs",
		`{"deployment_id":"surface_lifecycle","tenant_id":"fixture","agent_id":"fixture",`+
			`"provider":"local_postgres","provisioning_level":"schema","schema_name":"surface_lifecycle",`+
			`"backup_required":true,"lease_seconds":300}`)
	if status != 200 {
		t.Fatalf("provision=%d %s", status, body)
	}
	if n := surfaceCount(t, pool, `SELECT count(*) FROM pg_namespace WHERE nspname='surface_lifecycle'`); n != 1 {
		t.Fatalf("provisioned schemas=%d", n)
	}
	if n := surfaceCount(t, pool, `SELECT count(*) FROM sage.agent_db_deployments
  WHERE deployment_id='surface_lifecycle' AND status='active' AND provisioning_status='provisioned'`); n != 1 {
		t.Fatalf("durable deployment=%d", n)
	}
	for _, step := range []struct{ path, body string }{
		{"/ping", `{"status":"healthy","metrics":{"fixture":true}}`},
		{"/extend-lease", `{"lease_seconds":900,"reason":"fixture"}`},
		{"/archive", `{}`},
	} {
		status, body = f.request(t, "POST", root+step.path, step.body)
		if status != 200 {
			t.Fatalf("%s=%d %s", step.path, status, body)
		}
	}
	status, body = f.request(t, "DELETE", root, "")
	if status != 409 {
		t.Fatalf("missing restore verification delete=%d %s", status, body)
	}
	// G8-B11: restore_verified cannot be self-attested through the generic
	// backup endpoint; an admin records a restore drill with evidence.
	status, body = f.request(t, "POST", root+"/backups",
		`{"backup_id":"surface_backup","provider":"fixture","status":"restore_verified"}`)
	if status != 400 {
		t.Fatalf("self-attested restore_verified=%d %s", status, body)
	}
	status, body = f.request(t, "POST", root+"/backups/restore-drill",
		`{"backup_id":"surface_backup","evidence_uri":"s3://fixture/drill.json",`+
			`"target":"scratch","checks":["select 1"]}`)
	if status != 403 {
		t.Fatalf("operator restore-drill attestation=%d %s", status, body)
	}
	t.Run("admin_attests", func(t *testing.T) {
		f.login(t, "admin")
		status, body := f.request(t, "POST", root+"/backups/restore-drill",
			`{"backup_id":"surface_backup","evidence_uri":"s3://fixture/drill.json",`+
				`"target":"scratch","checks":["select 1"]}`)
		if status != 200 {
			t.Fatalf("admin restore-drill attestation=%d %s", status, body)
		}
	})
	status, body = f.request(t, "DELETE", root, "")
	if status != 200 || !strings.Contains(body, `"deleted":true`) {
		t.Fatalf("delete after attestation=%d %s", status, body)
	}
	if n := surfaceCount(t, pool, `SELECT count(*) FROM sage.agent_db_deployments
  WHERE deployment_id='surface_lifecycle' AND status='deleted'`); n != 1 {
		t.Fatalf("deleted rows=%d", n)
	}
	remaining := surfaceCount(t, pool, `SELECT count(*) FROM pg_namespace WHERE nspname='surface_lifecycle'`)
	t.Logf("real schema created; restore-required guard rejected delete; operator attestation allowed tombstone; remaining physical schemas=%d", remaining)
	// Existing contract is registry deletion: physical object removal is a separate unimplemented gate.
	if remaining != 1 {
		t.Errorf("unexpected physical schema mutation after registry deletion")
	}
}

func TestPreflightSurfaceAgentDBInvalidRegistrationLeavesNoSchema(t *testing.T) {
	pool := surfacePool(t)
	f := surfaceRouter(t, pool, config.DefaultConfig(), nil)
	f.login(t, "operator")
	status, body := f.request(t, "POST", "/api/v1/agent-dbs",
		`{"deployment_id":"surface_invalid","agent_id":"fixture",`+
			`"provider":"local_postgres","provisioning_level":"schema","schema_name":"surface_orphan"}`)
	if status != 400 {
		t.Fatalf("invalid input status=%d body=%s", status, body)
	}
	schemas := surfaceCount(t, pool, `SELECT count(*) FROM pg_namespace WHERE nspname='surface_orphan'`)
	rows := surfaceCount(t, pool, `SELECT count(*) FROM sage.agent_db_deployments
  WHERE deployment_id='surface_invalid'`)
	t.Logf("rejected registration status=%d physical schemas=%d registry rows=%d", status, schemas, rows)
	if schemas != 0 || rows != 0 {
		t.Error("rejected registration left a physical object without ownership record")
	}
}

func TestPreflightSurfaceAgentDBApprovalActorCannotBeSpoofed(t *testing.T) {
	pool := surfacePool(t)
	f := surfaceRouter(t, pool, config.DefaultConfig(), nil)
	actor := f.login(t, "operator")
	path := "/api/v1/agent-dbs/terraform-templates"
	status, body := f.request(t, "POST", path,
		`{"template_id":"surface_template","name":"local-fixture",`+
			`"files":[{"path":"main.tf","body":"resource \"aws_db_instance\" \"db\" {}"}],`+
			`"created_by":"forged-admin@fixture.invalid"}`)
	if status != 200 {
		t.Fatalf("template create=%d %s", status, body)
	}
	status, body = f.request(t, "POST", path+"/surface_template/approve",
		`{"approved_by":"forged-admin@fixture.invalid"}`)
	if status != 200 {
		t.Fatalf("template approve=%d %s", status, body)
	}
	var result struct {
		ApprovedBy string `json:"approved_by"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	persisted := surfaceCount(t, pool, `SELECT count(*) FROM sage.agent_db_terraform_templates
  WHERE template_id='surface_template' AND approved_by=$1`, result.ApprovedBy)
	t.Logf("authenticated actor=%s stored approved_by=%s persisted=%d", actor, result.ApprovedBy, persisted)
	if persisted != 1 {
		t.Fatal("response approval actor not durable")
	}
	if result.ApprovedBy != actor {
		t.Error("template approval audit identity trusts caller body over authenticated user")
	}
}
