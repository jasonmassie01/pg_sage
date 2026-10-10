package decommission

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var created = time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)

func cloud(id, provider string) deployment {
	return deployment{ID: id, Tenant: "t1", Provider: provider, Level: "instance",
		Status: "active", ProvisioningStatus: "available", CreatedAt: created}
}

func noEnv(string) (string, bool) { return "", false }

func build(st legacyState) Inventory {
	st.tablesPresent = true
	return buildInventory(st, noEnv, nil, created)
}

func itemsOfKind(inv Inventory, kind string) []Item {
	var out []Item
	for _, it := range inv.Items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

func findItem(t *testing.T, inv Inventory, id string) Item {
	t.Helper()
	for _, it := range inv.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("item %q not in inventory %+v", id, inv.Items)
	return Item{}
}

// G0-01: every row with evidence of a live call is selected, whatever its
// status; a row without any evidence is not.
func TestSelect_EveryLiveEvidenceSignalSelectsTheRowInEveryStatus(t *testing.T) {
	statuses := []string{"active", "failed", "provisioning", "status_unknown",
		"status_checked", "destroyed", "archived", "create_uncertain"}
	signals := map[string]func(d *deployment, st *legacyState){
		"live_mode":           func(d *deployment, _ *legacyState) { d.LiveMode = true },
		"create_operation_id": func(d *deployment, _ *legacyState) { d.CreateOperationID = "create:e1" },
		"provider_resource_id": func(d *deployment, _ *legacyState) {
			d.ResourceID = "pgsage-x"
		},
		"live_receipt": func(d *deployment, st *legacyState) {
			st.evidence[d.ID] = append(st.evidence[d.ID], "live_receipt")
		},
		"attempt:execute_live": func(d *deployment, st *legacyState) {
			st.evidence[d.ID] = append(st.evidence[d.ID], "attempt:execute_live")
		},
		"consumed_authorization:create": func(d *deployment, st *legacyState) {
			st.evidence[d.ID] = append(st.evidence[d.ID], "consumed_authorization:create")
		},
		"live_creation_receipt": func(d *deployment, st *legacyState) {
			st.evidence[d.ID] = append(st.evidence[d.ID], "live_creation_receipt")
		},
	}
	for signal, apply := range signals {
		for _, status := range statuses {
			d := cloud("dep-"+status, "gcp_cloudsql")
			d.Status, d.ProvisioningStatus = status, status
			st := legacyState{evidence: map[string][]string{}}
			apply(&d, &st)
			st.deployments = []deployment{d}
			inv := build(st)
			got := itemsOfKind(inv, KindProviderResource)
			if len(got) != 1 {
				t.Fatalf("%s in status %s: %d provider items, want 1", signal, status, len(got))
			}
			if !reflect.DeepEqual(got[0].Evidence, []string{signal}) {
				t.Fatalf("%s/%s evidence = %v", signal, status, got[0].Evidence)
			}
			if got[0].Status != status {
				t.Fatalf("status = %q, want %q", got[0].Status, status)
			}
		}
	}
}

func TestSelect_DryRunOnlyRowsAreNotInventoried(t *testing.T) {
	d := cloud("dep-dry", "aws_rds")
	d.ProvisioningStatus = "dry_run_ready"
	inv := build(legacyState{deployments: []deployment{d},
		evidence: map[string][]string{"dep-dry": nil}})
	if len(inv.Items) != 0 {
		t.Fatalf("a dry-run-only deployment was inventoried: %+v", inv.Items)
	}
	if inv.Items == nil {
		t.Fatal("Items must be an empty list, not null, in the JSON")
	}
}

func TestSelect_EvidenceIsSortedAndDeduplicated(t *testing.T) {
	d := cloud("dep-1", "neon")
	d.LiveMode, d.ResourceID = true, "br-1"
	inv := build(legacyState{deployments: []deployment{d}, evidence: map[string][]string{
		"dep-1": {"live_receipt", "attempt:execute_live", "live_receipt"}}})
	got := findItem(t, inv, "provider_resource:dep-1").Evidence
	want := []string{"attempt:execute_live", "live_mode", "live_receipt", "provider_resource_id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence = %v, want %v", got, want)
	}
}

func TestSelect_ResourceIDFallsBackToReceiptsThenTheDeterministicName(t *testing.T) {
	recorded := cloud("dep-rec", "aws_rds")
	recorded.ResourceID = "pgsage-dep-rec"
	viaLive := cloud("dep-live", "aws_rds")
	viaLive.LiveMode = true
	viaCreate := cloud("dep-create", "aws_rds")
	viaCreate.LiveMode = true
	uncertain := cloud("dep-1", "neon")
	uncertain.CreateOperationID = "create:e9"
	st := legacyState{
		deployments:  []deployment{recorded, viaLive, viaCreate, uncertain},
		evidence:     map[string][]string{},
		liveReceipts: map[string]string{"dep-live": "db-from-live-receipt"},
		receipts: map[string]creationReceipt{
			"dep-create": {ResourceID: "db-from-creation-receipt", Live: true},
		},
	}
	inv := build(st)
	for id, want := range map[string]string{
		"provider_resource:dep-rec":    "pgsage-dep-rec",
		"provider_resource:dep-live":   "db-from-live-receipt",
		"provider_resource:dep-create": "db-from-creation-receipt",
		"provider_resource:dep-1":      "",
	} {
		if got := findItem(t, inv, id).ResourceID; got != want {
			t.Errorf("%s resource id = %q, want %q", id, got, want)
		}
	}
	it := findItem(t, inv, "provider_resource:dep-1")
	if it.DeterministicName != "pgsage-dep-1-ee592263" {
		t.Fatalf("deterministic name = %q", it.DeterministicName)
	}
	if !strings.Contains(strings.Join(it.DeleteTemplate, "\n"), "pgsage-dep-1-ee592263") {
		t.Fatalf("an uncertain create's template must use the deterministic name: %v",
			it.DeleteTemplate)
	}
}

func TestSelect_RegionAndAccountFromParamsThenReceiptElseUnknown(t *testing.T) {
	params := cloud("dep-p", "gcp_cloudsql")
	params.LiveMode, params.Region, params.Project = true, "us-central1", "proj-a"
	receipt := cloud("dep-r", "aws_rds")
	receipt.LiveMode = true
	none := cloud("dep-n", "aws_rds")
	none.LiveMode = true
	neonProject := cloud("dep-np", "neon")
	neonProject.LiveMode, neonProject.Mode, neonProject.Organization = true, "project", "org-7"
	neonBranch := cloud("dep-nb", "neon")
	neonBranch.LiveMode, neonBranch.Project = true, "proj-9"
	st := legacyState{
		deployments: []deployment{params, receipt, none, neonProject, neonBranch},
		evidence:    map[string][]string{},
		receipts: map[string]creationReceipt{
			"dep-r": {Region: "us-east-2", Account: "123456789012"},
		},
	}
	inv := build(st)
	for id, want := range map[string][2]string{
		"provider_resource:dep-p":  {"us-central1", "proj-a"},
		"provider_resource:dep-r":  {"us-east-2", "123456789012"},
		"provider_resource:dep-n":  {"unknown", "unknown"},
		"provider_resource:dep-np": {"unknown", "org-7"},
		"provider_resource:dep-nb": {"unknown", "proj-9"},
	} {
		it := findItem(t, inv, id)
		if it.Region != want[0] || it.Account != want[1] {
			t.Errorf("%s region/account = %q/%q, want %q/%q",
				id, it.Region, it.Account, want[0], want[1])
		}
	}
}

func TestSelect_RDSFinalSnapshotOnlyForKeptRDSInstances(t *testing.T) {
	kept := cloud("dep-keep", "aws_rds")
	kept.ResourceID, kept.Status = "pgsage-dep-keep", "destroyed"
	disposable := cloud("dep-tmp", "aws_rds")
	disposable.ResourceID, disposable.Disposable = "pgsage-dep-tmp", true
	gcp := cloud("dep-gcp", "gcp_cloudsql")
	gcp.ResourceID = "pgsage-dep-gcp"
	inv := build(legacyState{deployments: []deployment{kept, disposable, gcp},
		evidence: map[string][]string{}})
	snaps := itemsOfKind(inv, KindRDSFinalSnapshot)
	if len(snaps) != 1 || snaps[0].DeploymentID != "dep-keep" {
		t.Fatalf("final snapshot items = %+v, want only dep-keep", snaps)
	}
	if snaps[0].DeterministicName != "pgsage-dep-keep-final-" {
		t.Fatalf("snapshot name prefix = %q", snaps[0].DeterministicName)
	}
}

func TestSelect_LocalArtifactsComeFromWhatPgSageCreated(t *testing.T) {
	schema := deployment{ID: "dep-s", Provider: "local_postgres", Level: "schema",
		Status: "active", SchemaName: "agentdb_s", CredentialScope: "agentdb_s",
		CreatedAt: created}
	dropped := deployment{ID: "dep-d", Provider: "local_postgres", Level: "schema",
		Status: "destroyed", SchemaName: "agentdb_gone", CredentialScope: "agentdb_gone",
		CreatedAt: created}
	database := deployment{ID: "dep-db", Provider: "local_postgres", Level: "database",
		Status: "active", DatabaseName: "agentdb_db", CredentialScope: "agentdb_db",
		CreatedAt: created}
	// Registered only: pg_sage never ran DDL for it, and database_name names
	// a database it does not own. It must never get a DROP DATABASE template.
	registered := deployment{ID: "dep-reg", Provider: "local_postgres", Level: "database",
		Status: "active", DatabaseName: "postgres", CreatedAt: created}
	inv := build(legacyState{
		deployments: []deployment{schema, dropped, database, registered},
		evidence:    map[string][]string{}, controlDB: "sage_meta",
		schemas:   map[string]bool{"agentdb_s": true},
		databases: map[string]bool{"agentdb_db": true},
	})
	if got := len(inv.Items); got != 3 {
		t.Fatalf("%d items, want 3: %+v", got, inv.Items)
	}
	s := findItem(t, inv, "local_schema:dep-s")
	if s.Present == nil || !*s.Present || s.ResourceID != "agentdb_s" {
		t.Fatalf("schema item = %+v", s)
	}
	d := findItem(t, inv, "local_schema:dep-d")
	if d.Present == nil || *d.Present {
		t.Fatalf("a dropped schema must report present=false: %+v", d)
	}
	db := findItem(t, inv, "local_database:dep-db")
	if db.Present == nil || !*db.Present || db.Account != "sage_meta" {
		t.Fatalf("database item = %+v", db)
	}
	for _, it := range inv.Items {
		if strings.Contains(strings.Join(it.DeleteTemplate, "\n"), `"postgres"`) {
			t.Fatalf("a registered-only row got a destructive template: %+v", it)
		}
	}
}

func TestSelect_SageSchemaInFleetAttachedAgentDatabases(t *testing.T) {
	env := cloud("dep-env", "neon")
	env.SecretRef, env.Host, env.ConnDatabase = "env:PG_SAGE_AGENTDB_DEP_ENV", "ep-1.neon.tech", "app"
	inline := cloud("dep-inline", "local_postgres")
	inline.Level, inline.Host, inline.Port, inline.ConnDatabase = "database", "db.internal", "6543", "agent"
	arn := cloud("dep-arn", "aws_rds")
	arn.SecretRef, arn.Host = "arn:aws:secretsmanager:us-east-2:1:secret:x", "x.rds.amazonaws.com"
	arn.ConnDatabase = "postgres"
	noConn := cloud("dep-none", "gcp_cloudsql")
	inv := build(legacyState{deployments: []deployment{env, inline, arn, noConn},
		evidence: map[string][]string{}})
	got := itemsOfKind(inv, KindAgentSageSchema)
	if len(got) != 2 {
		t.Fatalf("sage schema items = %+v, want dep-env and dep-inline", got)
	}
	e := findItem(t, inv, "agent_sage_schema:dep-env")
	if e.EnvRef != "env:PG_SAGE_AGENTDB_DEP_ENV" || e.ResourceID != "ep-1.neon.tech/app" {
		t.Fatalf("env-ref item = %+v", e)
	}
	i := findItem(t, inv, "agent_sage_schema:dep-inline")
	if i.ResourceID != "db.internal:6543/agent" || i.EnvRef != "" {
		t.Fatalf("inline item = %+v", i)
	}
}

func TestSelect_AcknowledgedItemsAreFlaggedAndCounted(t *testing.T) {
	a := cloud("dep-a", "neon")
	a.LiveMode = true
	b := cloud("dep-b", "neon")
	b.LiveMode = true
	inv := build(legacyState{deployments: []deployment{b, a}, evidence: map[string][]string{},
		acked: map[string]bool{"provider_resource:dep-a": true, "stale:gone": true}})
	if !findItem(t, inv, "provider_resource:dep-a").Acknowledged ||
		findItem(t, inv, "provider_resource:dep-b").Acknowledged {
		t.Fatalf("acknowledged flags wrong: %+v", inv.Items)
	}
	if inv.Unacknowledged != 1 {
		t.Fatalf("unacknowledged = %d, want 1", inv.Unacknowledged)
	}
	if inv.Items[0].ID != "provider_resource:dep-a" {
		t.Fatalf("items are not sorted by deployment: %v", inv.Items[0].ID)
	}
}

func TestSelect_NoLegacyTablesStillListsCredentials(t *testing.T) {
	inv := buildInventory(legacyState{}, func(name string) (string, bool) {
		return "x", name == "PG_SAGE_NEON_API_KEY"
	}, []string{"PG_SAGE_AGENTDB_ORPHAN=postgres://u:p@h/d"}, created)
	if inv.LegacyTables || len(inv.Items) != 0 || inv.Unacknowledged != 0 {
		t.Fatalf("empty install inventory = %+v", inv)
	}
	if inv.Spec != SpecRef || !inv.GeneratedAt.Equal(created) {
		t.Fatalf("spec/time = %q/%v", inv.Spec, inv.GeneratedAt)
	}
	setNames := map[string]bool{}
	for _, c := range inv.Credentials {
		if c.Set {
			setNames[c.Name] = true
		}
	}
	if !setNames["PG_SAGE_NEON_API_KEY"] || !setNames["PG_SAGE_AGENTDB_ORPHAN"] {
		t.Fatalf("set credentials = %v", setNames)
	}
	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "u:p@h") || strings.Contains(string(raw), `"x"`) {
		t.Fatalf("the inventory carries a secret value: %s", raw)
	}
}
