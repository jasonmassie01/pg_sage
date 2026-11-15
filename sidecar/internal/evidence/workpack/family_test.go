package workpack

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// evid-02a: a workload family is a statement's command plus its sorted
// referenced relations; a statement that does not parse (or whose text is
// unknown) is its own family. Ids derive from the key, never a row id.
// No DB or concurrency test here: family keys are pure functions.

var knownTables = map[string][]string{
	"orders":    {"public"},
	"customers": {"public"},
	"events":    {"app"},
}

func TestFamilyKey_CommandAndSortedRelations(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM orders WHERE id = $1":                         "select public.orders",
		"select o.* from customers c join orders o on o.c = c.id":    "select public.customers,public.orders",
		"  /* tag */ INSERT INTO app.events VALUES ($1)":             "insert app.events",
		"WITH x AS (SELECT 1 FROM customers) UPDATE orders SET a=1":  "update public.customers,public.orders",
		"(SELECT 1 FROM orders) UNION (SELECT 2 FROM orders)":        "select public.orders",
		"SELECT now()":                                               "select",
		"DELETE FROM unknown_table WHERE 'FROM orders' = $1":         "delete",
		"with recursive r(n) as (select 1) select n from r":          "select",
		"SELECT * FROM public.orders, customers WHERE true":          "select public.customers,public.orders",
		"VACUUM orders":                                              "vacuum",
		"WITH a AS (SELECT 1) , b AS (SELECT 2) DELETE FROM orders": "delete public.orders",
	}
	for text, want := range cases {
		if got := familyKey(42, text, knownTables); got != want {
			t.Errorf("familyKey(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestFamilyKey_UnparsedIsItsOwnFamily(t *testing.T) {
	for _, text := range []string{"", "   ", "/* only a comment */", "$1", "((("} {
		if got := familyKey(-77, text, knownTables); got != "unparsed:-77" {
			t.Errorf("familyKey(%q) = %q, want unparsed:-77", text, got)
		}
	}
}

func TestFamilyKey_NilCatalogLeavesRelationsOut(t *testing.T) {
	if got := familyKey(1, "SELECT * FROM orders", nil); got != "select" {
		t.Fatalf("with no catalog: %q, want select", got)
	}
}

func TestFamilyID_Is12HexOfTheKeyHash(t *testing.T) {
	sum := sha256.Sum256([]byte("select public.orders"))
	want := "workload/family/" + hex.EncodeToString(sum[:])[:12]
	if got := familyID("select public.orders"); got != want {
		t.Fatalf("familyID = %q, want %q", got, want)
	}
	if familyID("select public.orders") == familyID("select public.customers") {
		t.Fatal("two keys share an id")
	}
}

func TestQueryID_IsDecimalQueryid(t *testing.T) {
	if got := queryEvidenceID(-9223372036854775808); got !=
		"workload/query/-9223372036854775808" {
		t.Fatalf("queryEvidenceID = %q", got)
	}
}
