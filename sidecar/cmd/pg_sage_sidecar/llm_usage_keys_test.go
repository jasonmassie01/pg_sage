package main

import "testing"

// fleet-10: meta-DB databases are keyed by id, the rest by name.
func TestLLMUsageKeys(t *testing.T) {
	keys := newLLMUsageKeys()
	keys.setID("orders", 7)
	keys.setID("zero", 0)
	for database, want := range map[string]string{
		"": "process", "orders": "id:7", "zero": "name:zero", "plain": "name:plain",
	} {
		if got := keys.keyOf(database); got != want {
			t.Fatalf("keyOf(%q) = %q, want %q", database, got, want)
		}
	}
	for key, want := range map[string]string{"id:7": "orders", "name:plain": "plain"} {
		if got, ok := keys.nameOf(key); !ok || got != want {
			t.Fatalf("nameOf(%q) = (%q, %v), want %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"id:8", "id:x", "process", "", "name:"} {
		if got, ok := keys.nameOf(key); ok {
			t.Fatalf("nameOf(%q) = %q, want no database", key, got)
		}
	}
}
