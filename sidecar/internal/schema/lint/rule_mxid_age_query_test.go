package lint

import (
	"strings"
	"testing"
)

// relminmxid is a MultiXact ID, so its age must come from mxid_age().
// age() treats it as a transaction ID and, once MultiXact IDs outpace
// XIDs, reports a bogus ~2^31 age: PR #52 CI flagged a new table as
// "approaching wraparound" this way.
func TestMxidAgeQueryUsesMultiXactAge(t *testing.T) {
	query := mxidAgeQuery("'pg_catalog'")
	if !strings.Contains(query, "mxid_age(c.relminmxid)") {
		t.Fatalf("query does not use mxid_age(relminmxid):\n%s", query)
	}
	if strings.Contains(strings.ReplaceAll(query, "mxid_age(", ""), "age(c.relminmxid)") {
		t.Fatalf("query still applies XID age() to relminmxid:\n%s", query)
	}
}
