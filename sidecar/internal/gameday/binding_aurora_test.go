package gameday

import "testing"

// lab-05c: an Aurora copy-on-write clone copies the monitored database, so
// the customer programs run on it as on a DLE clone or a branch.
func TestHoldsCustomerObjects_AuroraClone(t *testing.T) {
	if !HoldsCustomerObjects("aurora_clone") {
		t.Fatal("an aurora_clone clone holds the monitored database's objects")
	}
	if HoldsCustomerObjects("aurora") || HoldsCustomerObjects("rds_restore") {
		t.Fatal("an unknown or not-yet-built provider is treated as a copy")
	}
}
