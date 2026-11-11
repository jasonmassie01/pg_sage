package slo

import (
	"encoding/json"
	"testing"
)

// playbooks-09 review: a fleet SLO with an owner_database pages only on
// its owner, so the SLOs page must know the owner to find the slo:<name>
// investigation; every status names it, evaluated or not.

func TestEvaluate_StatusNamesTheOwnerDatabase(t *testing.T) {
	o := appObjective()
	o.Database, o.OwnerDatabase = "orders", "billing"
	st := Evaluate(o, DefaultRules(), windowsAt(allBurning(20)), known(o.Window, 0.5), now)
	if st.OwnerDatabase != "billing" || st.Database != "orders" {
		t.Fatalf("owner/database = %q/%q, want billing/orders", st.OwnerDatabase, st.Database)
	}
	if st.State != StatePage {
		t.Fatalf("state = %s, want page (the owner must not change the verdict)", st.State)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["owner_database"] != "billing" {
		t.Fatalf("owner_database = %v, want billing", got["owner_database"])
	}
}

func TestEvaluate_StatusWithoutOwnerOmitsIt(t *testing.T) {
	o := appObjective()
	raw, err := json.Marshal(Evaluate(o, DefaultRules(), windowsAt(allBurning(0.1)),
		known(o.Window, 0.1), now))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, present := got["owner_database"]; present {
		t.Fatalf("owner_database present without an owner: %v", v)
	}
}

func TestUnevaluatedStatus_NamesTheOwnerDatabase(t *testing.T) {
	o := appObjective()
	o.OwnerDatabase = "billing"
	st := unevaluatedStatus(o, o.Hash())
	if st.OwnerDatabase != "billing" || st.State != StateUnknown {
		t.Fatalf("owner/state = %q/%s, want billing/unknown", st.OwnerDatabase, st.State)
	}
}

// No concurrent access tests: Evaluate and unevaluatedStatus are pure.
