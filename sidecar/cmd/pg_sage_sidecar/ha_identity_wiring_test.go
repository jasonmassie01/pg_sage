package main

import "testing"

// The earned-autonomy HA monitor persists its node identity under a key
// that survives restarts and never collides between databases: the
// meta-db record id when there is one, otherwise the instance name.
func TestHAIdentityKey(t *testing.T) {
	id := 7
	if got := haIdentityKey("orders", &id); got != "db:7" {
		t.Fatalf("key with a record id = %q, want db:7", got)
	}
	if got := haIdentityKey("orders", nil); got != "name:orders" {
		t.Fatalf("key by name = %q, want name:orders", got)
	}
	if haIdentityKey("orders", nil) == haIdentityKey("billing", nil) {
		t.Fatal("two databases share a key")
	}
	if got := haIdentityKey("", nil); got != "" {
		t.Fatalf("no database = %q, want no key (persistence off)", got)
	}
}
