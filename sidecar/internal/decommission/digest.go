package decommission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// itemIdentity is the part of an item that names what it is. Acknowledgement,
// presence and status are left out, so acknowledging or deleting a resource
// after the export does not make the exported digest stale; a new or changed
// item does.
type itemIdentity struct {
	ID, Kind, Provider, DeploymentID, ResourceID string
	DeterministicName, Region, Account, EnvRef   string
}

// inventoryDigest is the hex SHA-256 of the control database name and the
// identity of every item, in inventory order. The legacy table drop refuses
// unless the caller presents the digest of the inventory it reviewed.
func inventoryDigest(inv Inventory) string {
	ids := make([]itemIdentity, 0, len(inv.Items))
	for _, it := range inv.Items {
		ids = append(ids, itemIdentity{ID: it.ID, Kind: it.Kind, Provider: it.Provider,
			DeploymentID: it.DeploymentID, ResourceID: it.ResourceID,
			DeterministicName: it.DeterministicName, Region: it.Region,
			Account: it.Account, EnvRef: it.EnvRef})
	}
	// Marshalling strings and a slice of flat structs cannot fail.
	raw, _ := json.Marshal(struct {
		ControlDatabase string
		Items           []itemIdentity
	}{inv.ControlDatabase, ids})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
