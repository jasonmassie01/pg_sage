package policy

import (
	"context"
	"strings"
	"testing"
)

func TestNormalizeTargetObjectsCanonicalizesSortsAndDeduplicates(t *testing.T) {
	objects, err := NormalizeTargetObjects([]string{
		` PUBLIC.Orders `,
		`public.orders`,
		`analytics.events`,
	})
	if err != nil {
		t.Fatalf("NormalizeTargetObjects: %v", err)
	}
	want := []TargetObject{
		{Schema: "analytics", Name: "events", Canonical: "analytics.events"},
		{Schema: "public", Name: "orders", Canonical: "public.orders"},
	}
	if len(objects) != len(want) {
		t.Fatalf("objects = %#v, want %#v", objects, want)
	}
	for i := range want {
		if objects[i] != want[i] {
			t.Fatalf("objects[%d] = %#v, want %#v", i, objects[i], want[i])
		}
	}
}

func TestNormalizeTargetObjectsPreservesQuotedIdentifierIdentity(t *testing.T) {
	objects, err := NormalizeTargetObjects([]string{`"Sales"."OrderItems"`})
	if err != nil {
		t.Fatalf("NormalizeTargetObjects: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("len(objects) = %d, want 1", len(objects))
	}
	want := TargetObject{
		Schema: "Sales", Name: "OrderItems", Canonical: `"Sales"."OrderItems"`,
	}
	if objects[0] != want {
		t.Fatalf("object = %#v, want %#v", objects[0], want)
	}
}

func TestNormalizeTargetObjectsRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	for _, input := range []string{
		"", "orders", "public.orders.extra", "public.orders; DROP TABLE users",
		"public.*", `"unterminated.orders`,
	} {
		t.Run(input, func(t *testing.T) {
			_, err := NormalizeTargetObjects([]string{input})
			if err == nil || !strings.Contains(err.Error(), "target object") {
				t.Fatalf("NormalizeTargetObjects(%q) error = %v", input, err)
			}
		})
	}
}

func TestLeaseKeyIsStableAcrossEquivalentInputOrdering(t *testing.T) {
	first, err := NormalizeTargetObjects([]string{"public.orders", "public.customers"})
	if err != nil {
		t.Fatalf("normalize first: %v", err)
	}
	second, err := NormalizeTargetObjects([]string{"PUBLIC.Customers", "public.orders"})
	if err != nil {
		t.Fatalf("normalize second: %v", err)
	}

	firstKey := LeaseKey(first)
	secondKey := LeaseKey(second)

	if firstKey == "" {
		t.Fatal("LeaseKey returned empty key")
	}
	if firstKey != secondKey {
		t.Fatalf("equivalent object sets produced keys %q and %q", firstKey, secondKey)
	}
}

func TestLeaseManagerInterfaceCarriesNormalizedObjectsAndTypedID(t *testing.T) {
	objects := []TargetObject{
		{Schema: "public", Name: "orders", Canonical: "public.orders"},
	}
	manager := &recordingLeaseManager{}

	leaseID, err := manager.AcquireLease(
		context.Background(), "sidecar-a", objects, "create_index")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if leaseID != LeaseID("lease-1") {
		t.Fatalf("LeaseID = %q", leaseID)
	}
	if manager.actor != "sidecar-a" || manager.intent != "create_index" {
		t.Fatalf("recorded actor/intent = %q/%q", manager.actor, manager.intent)
	}
	if len(manager.objects) != 1 || manager.objects[0] != objects[0] {
		t.Fatalf("recorded objects = %#v", manager.objects)
	}
	if err := manager.ReleaseLease(context.Background(), leaseID); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if manager.released != leaseID {
		t.Fatalf("released = %q, want %q", manager.released, leaseID)
	}
}

type recordingLeaseManager struct {
	actor    string
	objects  []TargetObject
	intent   string
	released LeaseID
}

func (m *recordingLeaseManager) AcquireLease(
	_ context.Context,
	actor string,
	objects []TargetObject,
	intent string,
) (LeaseID, error) {
	m.actor = actor
	m.objects = append([]TargetObject(nil), objects...)
	m.intent = intent
	return LeaseID("lease-1"), nil
}

func (m *recordingLeaseManager) ReleaseLease(
	_ context.Context,
	id LeaseID,
) error {
	m.released = id
	return nil
}

var _ LeaseManager = (*recordingLeaseManager)(nil)
