package sre

import (
	"strings"
	"testing"
)

// The Postgres-specialist contract redacts what leaves pg_sage with the
// replay export's rules: secrets and PII always, identifiers hashed unless
// kept.

func TestScrub_RemovesSecretsAndPII(t *testing.T) {
	in := "password=hunter2 bob@example.com +1 415-555-0100 sk-abcdefghijklmnopqrstuv " +
		"postgres://u:p@h/db 123-45-6789 public.orders pid 4242"
	out := Scrub(in)
	for _, leak := range []string{"hunter2", "bob@example.com", "555-0100",
		"sk-abcdefghijklmnopqrstuv", "u:p@", "123-45-6789"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q survived: %s", leak, out)
		}
	}
	if !strings.Contains(out, "public.orders") || !strings.Contains(out, "pid 4242") {
		t.Fatalf("identifiers and numbers are kept: %s", out)
	}
	if Scrub("") != "" {
		t.Fatal("empty")
	}
}

func TestIdentifierRedactor(t *testing.T) {
	keep := NewIdentifierRedactor(true, nil)
	if got := keep.Text(`waits on "Orders" and public.orders, token=abc`); !strings.Contains(
		got, "public.orders") || strings.Contains(got, "abc") {
		t.Fatalf("keep: %s", got)
	}
	if keep.Name("public.orders") != "public.orders" {
		t.Fatal("kept names stay")
	}
	hash := NewIdentifierRedactor(false, []byte("k1"))
	a := hash.Text(`blocks public.orders and "Orders"`)
	if strings.Contains(a, "public.orders") || strings.Contains(a, "Orders") ||
		!strings.Contains(a, "id_") {
		t.Fatalf("hashed: %s", a)
	}
	if hash.Name("public.orders") != hash.Name("public.orders") ||
		!strings.Contains(a, hash.Name("public.orders")) {
		t.Fatal("one key hashes a name the same way in text and fields")
	}
	other := NewIdentifierRedactor(false, []byte("k2"))
	if other.Name("public.orders") == hash.Name("public.orders") {
		t.Fatal("another key gives unlinkable hashes")
	}
	if len(ScrubRules) <= len(RedactionRules) {
		t.Fatalf("scrub rules extend the export rules: %v", ScrubRules)
	}
}
