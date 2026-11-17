package gameday

import "testing"

// lab-05b: a Neon branch copies the monitored database, so the customer
// programs run on it as on a DLE clone or a Lakebase branch.
func TestHoldsCustomerObjects_NeonBranch(t *testing.T) {
	if !HoldsCustomerObjects("neon_branch") {
		t.Fatal("a neon_branch clone holds the monitored database's objects")
	}
	if HoldsCustomerObjects("neon") || HoldsCustomerObjects("") {
		t.Fatal("an unknown provider is treated as a copy")
	}
}
