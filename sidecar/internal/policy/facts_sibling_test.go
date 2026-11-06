package policy

import (
	"context"
	"strings"
	"testing"
)

// facts-02: a slot fact confirmed on a sibling database of the same
// cluster binds here too; the detail names that database, because this
// database has no fact with that id.
func TestGateFactDetailNamesTheSiblingDatabase(t *testing.T) {
	sibling := FactBinding{FactID: 7, Type: "slot_consumer", Subject: "cdc_*",
		Route: "alert", Object: "slot:*", Database: "orders_a",
		Summary:     "slot cdc_* feeds debezium",
		ConfirmedBy: "alice@example.com", ConfirmedAt: factConfirmedAt}
	binder := &fakeBinder{bindings: []FactBinding{appOwnedBinding(), sibling}}
	d := newFactGate(t, gateFixture{}, binder).Authorize(context.Background(),
		validIndexRequest())
	assertDecision(t, d, VerdictBlocked, ReasonBoundByFact)
	for _, want := range []string{"fact #12: table public.orders",
		`fact #7 of database "orders_a": slot cdc_* feeds debezium`, "route: alert"} {
		if !strings.Contains(d.Detail, want) {
			t.Fatalf("detail %q lacks %q", d.Detail, want)
		}
	}
	if strings.Contains(d.Detail, `fact #12 of database`) {
		t.Fatalf("a local fact is named as a sibling's: %q", d.Detail)
	}
}
