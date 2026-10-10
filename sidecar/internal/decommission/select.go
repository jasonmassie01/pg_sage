package decommission

import (
	"sort"
	"strings"
)

const (
	providerLocal    = "local_postgres"
	providerRDS      = "aws_rds"
	providerCloudSQL = "gcp_cloudsql"
	providerLakebase = "databricks_lakebase"
	providerNeon     = "neon"
	providerSupabase = "supabase"
)

// itemsFor selects the items one deployment contributes. A cloud deployment
// is selected by evidence of any live call, never by its status (G0-01).
func itemsFor(d deployment, st legacyState) []Item {
	var items []Item
	if d.Provider != providerLocal {
		if evidence := liveEvidence(d, st.evidence[d.ID]); len(evidence) > 0 {
			res := providerItem(d, st, evidence)
			items = append(items, res)
			if d.Provider == providerRDS && !d.Disposable {
				items = append(items, rdsSnapshotItem(res))
			}
		}
	}
	if it, ok := localItem(d, st); ok {
		items = append(items, it)
	}
	if it, ok := sageSchemaItem(d); ok {
		items = append(items, it)
	}
	return items
}

// liveEvidence lists the row's own evidence plus what other tables hold,
// sorted and without duplicates.
func liveEvidence(d deployment, external []string) []string {
	seen := map[string]bool{}
	add := func(label string) { seen[label] = true }
	if d.LiveMode {
		add("live_mode")
	}
	if d.CreateOperationID != "" {
		add("create_operation_id")
	}
	if d.ResourceID != "" {
		add("provider_resource_id")
	}
	for _, label := range external {
		add(label)
	}
	out := make([]string, 0, len(seen))
	for label := range seen {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func baseItem(d deployment, kind string) Item {
	return Item{ID: kind + ":" + d.ID, Kind: kind, Provider: d.Provider,
		DeploymentID: d.ID, Status: d.Status, ProvisioningStatus: d.ProvisioningStatus,
		CreatedAt: d.CreatedAt, Region: unknown, Account: unknown, Evidence: []string{}}
}

func providerItem(d deployment, st legacyState, evidence []string) Item {
	it := baseItem(d, KindProviderResource)
	it.Evidence = evidence
	it.DeterministicName = deterministicName(d.Provider, d.ID)
	it.Mode = hostedMode(d)
	receipt := st.receipts[d.ID]
	it.ResourceID = firstNonEmpty(d.ResourceID, st.liveReceipts[d.ID], receipt.ResourceID)
	it.Region = firstNonEmpty(d.Region, receipt.Region, unknown)
	it.Account = firstNonEmpty(accountOf(d), receipt.Account, unknown)
	return it
}

// hostedMode is the Neon or Supabase mode: a branch of a project (the
// default) or a whole project in an organization.
func hostedMode(d deployment) string {
	if d.Provider != providerNeon && d.Provider != providerSupabase {
		return ""
	}
	if strings.TrimSpace(d.Mode) == "project" {
		return "project"
	}
	return "branch"
}

// accountOf is where the resource lives: the GCP or Lakebase project, the
// hosted project (branch mode) or organization (project mode).
func accountOf(d deployment) string {
	switch d.Provider {
	case providerNeon, providerSupabase:
		if hostedMode(d) == "project" {
			return d.Organization
		}
		return d.Project
	case providerCloudSQL, providerLakebase:
		return d.Project
	}
	return ""
}

// rdsSnapshotItem covers the final snapshot a destroy of a kept (not
// disposable) RDS instance took. Its name carries the destroy time, so the
// item names the deterministic prefix.
func rdsSnapshotItem(instance Item) Item {
	it := instance
	it.ID = KindRDSFinalSnapshot + ":" + instance.DeploymentID
	it.Kind = KindRDSFinalSnapshot
	id := firstNonEmpty(instance.ResourceID, instance.DeterministicName)
	it.ResourceID = ""
	it.DeterministicName = id + "-final-"
	return it
}

// localItem is the schema or database pg_sage itself created on the control
// database. Only a recorded credential_scope proves the CREATE ran, so a row
// that merely names an existing database never gets a DROP template.
func localItem(d deployment, st legacyState) (Item, bool) {
	if d.Provider != providerLocal || d.CredentialScope == "" {
		return Item{}, false
	}
	var it Item
	var present bool
	switch {
	case d.Level == "schema" && d.SchemaName == d.CredentialScope:
		it = baseItem(d, KindLocalSchema)
		present = st.schemas[d.SchemaName]
	case d.Level == "database" && d.DatabaseName == d.CredentialScope:
		it = baseItem(d, KindLocalDatabase)
		present = st.databases[d.DatabaseName]
	default:
		return Item{}, false
	}
	it.ResourceID = d.CredentialScope
	it.Evidence = []string{"credential_scope"}
	it.Account = firstNonEmpty(st.controlDB, unknown)
	it.Present = &present
	return it, true
}

// sageSchemaItem is the sage schema pg_sage's own bootstrap left in an agent
// database that fleet sync attached: one with an env: DSN reference, or with
// inline connection info and no other secret reference.
func sageSchemaItem(d deployment) (Item, bool) {
	envRef := ""
	switch {
	case strings.HasPrefix(d.SecretRef, "env:"):
		envRef = d.SecretRef
	case d.SecretRef != "" || d.Host == "" || d.ConnDatabase == "":
		return Item{}, false
	}
	it := baseItem(d, KindAgentSageSchema)
	it.EnvRef = envRef
	it.Evidence = []string{"fleet_connection"}
	location := firstNonEmpty(d.Host, unknown)
	if d.Port != "" {
		location += ":" + d.Port
	}
	it.ResourceID = location + "/" + firstNonEmpty(d.ConnDatabase, d.DatabaseName)
	return it, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
