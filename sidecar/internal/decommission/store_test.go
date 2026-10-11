package decommission

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// insertDeployment writes one legacy deployment row; cols overrides columns.
func insertDeployment(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id, provider, status string, cols map[string]any) {
	t.Helper()
	row := map[string]any{"deployment_id": id, "tenant_id": "t1", "agent_id": "a1",
		"provider": provider, "status": status, "provisioning_level": "instance"}
	for k, v := range cols {
		row[k] = v
	}
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]any, len(keys))
	marks := make([]string, len(keys))
	for i, k := range keys {
		args[i] = row[k]
		marks[i] = fmt.Sprintf("$%d", i+1)
	}
	mustExec(t, ctx, pool, "INSERT INTO sage.agent_db_deployments ("+
		strings.Join(keys, ",")+") VALUES ("+strings.Join(marks, ",")+")", args...)
}

func liveReceiptChain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, dep, res string) {
	t.Helper()
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_live_plans (plan_hash, deployment_id,
		payload) VALUES ($1, $2, '{}')`, "plan-"+dep, dep)
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_live_estimates (estimate_id, plan_hash,
		deployment_id, payload, expires_at, consumed_at) VALUES ($1, $2, $3, '{}',
		now() + interval '1 hour', now())`, "est-"+dep, "plan-"+dep, dep)
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_live_authorizations (authorization_id,
		estimate_id, plan_hash, deployment_id, operation, requester_id, idempotency_key,
		payload, expires_at, consumed_at) VALUES ($1, $2, $3, $4, 'create', 'admin', $5, '{}',
		now() + interval '1 hour', now())`, "auth-"+dep, "est-"+dep, "plan-"+dep, dep, "k-"+dep)
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_live_receipts (authorization_id,
		idempotency_key, plan_hash, payload) VALUES ($1, $2, $3, $4)`,
		"auth-"+dep, "k-"+dep, "plan-"+dep,
		`{"AuthorizationID":"auth-`+dep+`","ProviderResourceID":"`+res+`"}`)
}

func seedEstate(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	insertDeployment(t, ctx, pool, "d-live", "aws_rds", "failed", map[string]any{
		"live_mode": true, "provider_resource_id": "pgsage-d-live",
		"metadata": `{"provider_params":{"region":"us-east-2"}}`})
	insertDeployment(t, ctx, pool, "d-op", "gcp_cloudsql", "provisioning", map[string]any{
		"create_operation_id": "create:est-9", "provisioning_status": "create_uncertain",
		"metadata": `{"provider_params":{"project":"proj-a","region":"us-central1"}}`})
	insertDeployment(t, ctx, pool, "d-receipt", "neon", "destroyed", map[string]any{
		"metadata": `{"provider_params":{"mode":"branch","project":"proj-9"}}`})
	liveReceiptChain(t, ctx, pool, "d-receipt", "br-from-receipt")
	insertDeployment(t, ctx, pool, "d-attempt", "supabase", "status_unknown", nil)
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_provision_attempts (deployment_id,
		kind, status, runner) VALUES ('d-attempt', 'execute_live', 'failed', 'supabase')`)
	insertDeployment(t, ctx, pool, "d-creation", "databricks_lakebase", "status_checked", nil)
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_creation_receipts (deployment_id,
		provider, provider_resource_id, region, account_ref, operation_mode) VALUES
		('d-creation', 'databricks_lakebase', 'lb-branch-1', 'westus', 'lake-proj', 'live')`)
	insertDeployment(t, ctx, pool, "d-dry", "aws_rds", "active", map[string]any{
		"provisioning_status": "dry_run_ready"})
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_provision_attempts (deployment_id,
		kind, status, runner) VALUES ('d-dry', 'execute', 'succeeded', 'dry_run')`)
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_creation_receipts (deployment_id,
		provider, provider_resource_id, operation_mode) VALUES
		('d-dry', 'aws_rds', 'dry-run-id', 'dry_run')`)
	mustExec(t, ctx, pool, `CREATE SCHEMA agentdb_local_s`)
	insertDeployment(t, ctx, pool, "d-local", "local_postgres", "active", map[string]any{
		"provisioning_level": "schema", "schema_name": "agentdb_local_s",
		"metadata": `{"credential_scope":"agentdb_local_s"}`})
	insertDeployment(t, ctx, pool, "d-fleet", "local_postgres", "archived", map[string]any{
		"provisioning_level": "database", "secret_ref": "env:PG_SAGE_AGENTDB_D_FLEET",
		"connection_info": `{"host":"agent.internal","database":"agentapp",` +
			`"password":"inline-secret-value"}`})
}

func itemIDs(inv Inventory) []string {
	ids := make([]string, 0, len(inv.Items))
	for _, it := range inv.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

// G0-01 against real tables: selection by evidence, across statuses.
func TestBuild_SelectsTheLegacyEstateByEvidence(t *testing.T) {
	pool, ctx := legacyDB(t, "decom_estate")
	seedEstate(t, ctx, pool)
	inv, err := Build(ctx, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []string{
		"provider_resource:d-attempt", "provider_resource:d-creation",
		"agent_sage_schema:d-fleet",
		"provider_resource:d-live", "rds_final_snapshot:d-live", "local_schema:d-local",
		"provider_resource:d-op", "provider_resource:d-receipt",
	}
	if got := itemIDs(inv); !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %v\nwant    %v", got, want)
	}
	if !inv.LegacyTables || inv.Unacknowledged != len(want) {
		t.Fatalf("legacy=%v unacknowledged=%d", inv.LegacyTables, inv.Unacknowledged)
	}
	checks := map[string]func(Item) bool{
		"provider_resource:d-receipt": func(it Item) bool {
			return it.ResourceID == "br-from-receipt" && it.Status == "destroyed" &&
				reflect.DeepEqual(it.Evidence,
					[]string{"consumed_authorization:create", "live_receipt"})
		},
		"provider_resource:d-attempt": func(it Item) bool {
			return reflect.DeepEqual(it.Evidence, []string{"attempt:execute_live"}) &&
				it.DeterministicName == "pgsage-d-attempt-"+nameDigest("d-attempt")
		},
		"provider_resource:d-creation": func(it Item) bool {
			return it.ResourceID == "lb-branch-1" && it.Region == "westus" &&
				it.Account == "lake-proj"
		},
		"provider_resource:d-op": func(it Item) bool {
			return it.Account == "proj-a" && it.Region == "us-central1" &&
				it.ProvisioningStatus == "create_uncertain"
		},
		"local_schema:d-local": func(it Item) bool {
			return it.Present != nil && *it.Present && it.ResourceID == "agentdb_local_s"
		},
		"agent_sage_schema:d-fleet": func(it Item) bool {
			return it.EnvRef == "env:PG_SAGE_AGENTDB_D_FLEET" &&
				it.ResourceID == "agent.internal/agentapp"
		},
	}
	for _, it := range inv.Items {
		if check, ok := checks[it.ID]; ok && !check(it) {
			t.Errorf("%s = %+v", it.ID, it)
		}
		if it.CreatedAt.IsZero() || len(it.DeleteTemplate) == 0 {
			t.Errorf("%s lacks created_at or a template", it.ID)
		}
	}
	raw, _ := json.Marshal(inv)
	if strings.Contains(string(raw), "inline-secret-value") {
		t.Fatal("the inventory leaked an inline connection password")
	}
}

func TestBuild_NoLegacyTables(t *testing.T) {
	pool, ctx := freshDB(t, "decom_empty")
	inv, err := Build(ctx, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if inv.LegacyTables || len(inv.Items) != 0 || inv.Items == nil {
		t.Fatalf("inventory of a clean install = %+v", inv)
	}
	if inv.ControlDatabase == "" {
		t.Fatal("control database name missing")
	}
}

// An install upgraded straight from an older AgentDB schema lacks later
// columns and tables; the inventory still reads what exists.
func TestBuild_OlderSchemaVersions(t *testing.T) {
	pool, ctx := freshDB(t, "decom_old")
	mustExec(t, ctx, pool, `CREATE SCHEMA sage;
		CREATE TABLE sage.agent_db_deployments (deployment_id text PRIMARY KEY,
			tenant_id text NOT NULL, agent_id text NOT NULL, status text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			metadata jsonb NOT NULL DEFAULT '{}')`)
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_deployments (deployment_id, tenant_id,
		agent_id, status) VALUES ('old-1', 't', 'a', 'active')`)
	inv, err := Build(ctx, pool)
	if err != nil {
		t.Fatalf("Build on the oldest shape: %v", err)
	}
	if !inv.LegacyTables || len(inv.Items) != 0 {
		t.Fatalf("oldest shape inventory = %+v", inv)
	}
	mustExec(t, ctx, pool, `ALTER TABLE sage.agent_db_deployments
		ADD COLUMN provider text NOT NULL DEFAULT 'local_postgres',
		ADD COLUMN provider_resource_id text NOT NULL DEFAULT '',
		ADD COLUMN live_mode boolean NOT NULL DEFAULT false`)
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_deployments (deployment_id, tenant_id,
		agent_id, status, provider, provider_resource_id) VALUES
		('mid-1', 't', 'a', 'failed', 'aws_rds', 'pgsage-mid-1')`)
	inv, err = Build(ctx, pool)
	if err != nil {
		t.Fatalf("Build on a middle shape: %v", err)
	}
	if got := itemIDs(inv); !reflect.DeepEqual(got, []string{
		"provider_resource:mid-1", "rds_final_snapshot:mid-1"}) {
		t.Fatalf("middle shape items = %v", got)
	}
}

func TestBuild_ErrorsNameWhatFailed(t *testing.T) {
	pool, ctx := legacyDB(t, "decom_err")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Build(cctx, pool); err == nil || !strings.Contains(err.Error(), "inventory") {
		t.Fatalf("canceled Build err = %v, want an inventory error", err)
	}
	pool.Close()
	if _, err := Build(ctx, pool); err == nil {
		t.Fatal("Build on a closed pool succeeded")
	}
	if _, err := Build(ctx, nil); err == nil || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("nil pool err = %v", err)
	}
}

// The inventory is read-only: building it twice changes nothing.
func TestBuild_IsReadOnly(t *testing.T) {
	pool, ctx := legacyDB(t, "decom_ro")
	seedEstate(t, ctx, pool)
	count := func() (n int64) {
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sage.agent_db_deployments)
			+ (SELECT count(*) FROM pg_namespace WHERE nspname = 'agentdb_local_s')`).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	start := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := Build(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if after := count(); after != before {
		t.Fatalf("rows/schemas changed %d -> %d", before, after)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("two inventories took %v", time.Since(start))
	}
}
