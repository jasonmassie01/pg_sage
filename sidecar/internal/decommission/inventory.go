// Package decommission is what remains of AgentDB provisioning after its
// removal (AGENTDB-SPEC §12, decision D-1): a read-only inventory of every
// resource the removed provisioner may have created and billed, per-provider
// delete templates, the operator's acknowledgement, and the startup checks
// for the removed configuration. It never deletes anything live: pg_sage
// must never forget a billed resource it created, and it no longer holds the
// authority to destroy one.
package decommission

import (
	"sort"
	"time"
)

// SpecRef names the specification section operators follow.
const SpecRef = "AGENTDB-SPEC §12"

// unknown is reported where the removed provisioner never recorded a value.
const unknown = "unknown"

// Item kinds, in the order an operator works through one deployment.
const (
	KindProviderResource = "provider_resource"
	KindRDSFinalSnapshot = "rds_final_snapshot"
	KindLocalSchema      = "local_schema"
	KindLocalDatabase    = "local_database"
	KindAgentSageSchema  = "agent_sage_schema"
)

var kindOrder = map[string]int{KindProviderResource: 0, KindRDSFinalSnapshot: 1,
	KindLocalSchema: 2, KindLocalDatabase: 3, KindAgentSageSchema: 4}

// Inventory is everything the removed provisioner may have left behind.
type Inventory struct {
	GeneratedAt time.Time `json:"generated_at"`
	Spec        string    `json:"spec"`
	// LegacyTables reports whether sage.agent_db_deployments exists.
	LegacyTables    bool         `json:"legacy_tables"`
	ControlDatabase string       `json:"control_database"`
	Items           []Item       `json:"items"`
	Credentials     []Credential `json:"credentials"`
	Unacknowledged  int          `json:"unacknowledged"`
}

// Item is one resource to review and, if it still exists, delete by hand.
// It never carries a secret value: connection passwords are not read, and
// environment references are names only.
type Item struct {
	ID                 string    `json:"id"`
	Kind               string    `json:"kind"`
	Provider           string    `json:"provider"`
	DeploymentID       string    `json:"deployment_id"`
	ResourceID         string    `json:"resource_id"`
	DeterministicName  string    `json:"deterministic_name"`
	Mode               string    `json:"mode,omitempty"`
	Region             string    `json:"region"`
	Account            string    `json:"account"`
	Status             string    `json:"status"`
	ProvisioningStatus string    `json:"provisioning_status"`
	CreatedAt          time.Time `json:"created_at"`
	Evidence           []string  `json:"evidence"`
	Present            *bool     `json:"present,omitempty"`
	EnvRef             string    `json:"env_ref,omitempty"`
	DeleteTemplate     []string  `json:"delete_template"`
	Acknowledged       bool      `json:"acknowledged"`
}

// deployment is the part of one legacy deployment row the inventory reads.
type deployment struct {
	ID, Tenant, Provider, Level, Status, ProvisioningStatus string
	DatabaseName, SchemaName, ResourceID, CreateOperationID string
	SecretRef                                               string
	LiveMode, Disposable                                    bool
	CreatedAt                                               time.Time
	Region, Project, Organization, Mode, CredentialScope    string
	Host, Port, ConnDatabase                                string
}

// creationReceipt is a row of sage.agent_db_creation_receipts.
type creationReceipt struct {
	ResourceID, Region, Account string
	Live                        bool
}

// legacyState is everything read from the control database.
type legacyState struct {
	tablesPresent bool
	controlDB     string
	deployments   []deployment
	// evidence holds live-call evidence found in other tables, by deployment.
	evidence map[string][]string
	// liveReceipts maps a deployment to the resource id its live receipt holds.
	liveReceipts map[string]string
	receipts     map[string]creationReceipt
	// schemas and databases are the local artifact names that exist now.
	schemas, databases map[string]bool
	acked              map[string]bool
}

// buildInventory turns the legacy state into the inventory. lookupEnv and
// environ are the process environment (injected for tests).
func buildInventory(
	st legacyState,
	lookupEnv func(string) (string, bool),
	environ []string,
	now time.Time,
) Inventory {
	inv := Inventory{GeneratedAt: now.UTC(), Spec: SpecRef, LegacyTables: st.tablesPresent,
		ControlDatabase: st.controlDB, Items: []Item{}}
	var envRefs []string
	for _, d := range st.deployments {
		inv.Items = append(inv.Items, itemsFor(d, st)...)
		envRefs = append(envRefs, d.SecretRef)
	}
	sort.SliceStable(inv.Items, func(i, j int) bool {
		a, b := inv.Items[i], inv.Items[j]
		if a.DeploymentID != b.DeploymentID {
			return a.DeploymentID < b.DeploymentID
		}
		return kindOrder[a.Kind] < kindOrder[b.Kind]
	})
	for i := range inv.Items {
		inv.Items[i].DeleteTemplate = deleteTemplate(inv.Items[i])
		inv.Items[i].Acknowledged = st.acked[inv.Items[i].ID]
		if !inv.Items[i].Acknowledged {
			inv.Unacknowledged++
		}
	}
	inv.Credentials = credentials(lookupEnv, environ, envRefs)
	return inv
}
