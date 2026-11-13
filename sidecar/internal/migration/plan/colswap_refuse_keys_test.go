package plan

import "testing"

// A referencing key that is NOT VALID (or, on PostgreSQL 18, NOT
// ENFORCED) would come back enforced and then be validated: rows it
// tolerated today would fail the VALIDATE step, and writes it lets
// through today would start failing. The swap refuses it.
func TestColumnSwapRefusesUnvalidatedReferencingFK(t *testing.T) {
	facts := pkFacts()
	facts.Referencing[0].NotValidated = true
	refuse(t, facts, intReq(), "lines_order_fk is not validated")
}

// The primary key the swap re-attaches is a plain, immediate unique
// index on the new column. A deferrable key, one with INCLUDE columns or
// one that is the replica identity would come back different, so the
// swap refuses each of them.
func TestColumnSwapRefusesPrimaryKeyItCannotRebuild(t *testing.T) {
	for name, mutate := range map[string]func(*SwapKey){
		"deferrable":       func(k *SwapKey) { k.Deferrable = true },
		"INCLUDE columns":  func(k *SwapKey) { k.Include = true },
		"replica identity": func(k *SwapKey) { k.ReplicaIdentity = true },
	} {
		facts := pkFacts()
		mutate(facts.PrimaryKey)
		refuse(t, facts, intReq(), name)
	}
}

// Boundary: those primary key shapes only matter when the swap moves the
// key. A non-key column of a table whose key is deferrable, has INCLUDE
// columns and is the replica identity still plans: the backfill only
// walks that key.
func TestColumnSwapKeepsUnusualPrimaryKeyOfOtherColumn(t *testing.T) {
	facts := textFacts()
	facts.PrimaryKey.Deferrable = true
	facts.PrimaryKey.Include = true
	facts.PrimaryKey.ReplicaIdentity = true
	p := mustSwap(t, facts, SwapRequest{Schema: "public", Table: "events", Column: "ref",
		TargetType: "bigint"})
	for _, s := range p.Steps {
		if s.Name == "attach_primary_key" || s.Name == "drop_primary_key" {
			t.Fatalf("swap of a non-key column touches the primary key: step %s", s.Name)
		}
	}
	if got := stepByName(t, p, "backfill").SQL; got == "" {
		t.Fatal("backfill step has no SQL")
	}
}
