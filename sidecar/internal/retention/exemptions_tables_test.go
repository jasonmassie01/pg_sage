package retention

import (
	"sort"
	"testing"
)

// agentgov-04: an exemption must name a table the migrations create; a
// stale one hides that a table was dropped (sage.guard_principal_files)
// or misspelled, and would let a later table of that name skip the
// coverage guard without a retention decision.

func TestRetentionExemptions_NameExistingTables(t *testing.T) {
	pool, ctx := requireDB(t)
	names := make([]string, 0, len(retentionExemptions))
	for name := range retentionExemptions {
		names = append(names, name)
	}
	sort.Strings(names)
	rows, err := pool.Query(ctx, `SELECT n FROM unnest($1::text[]) AS n
		WHERE to_regclass(quote_ident('sage') || '.' || quote_ident(n)) IS NULL`, names)
	if err != nil {
		t.Fatalf("look up exempt tables: %v", err)
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan exempt table: %v", err)
		}
		missing = append(missing, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read exempt tables: %v", err)
	}
	if len(names) < 50 {
		t.Fatalf("only %d exemptions; the merged map lost a group", len(names))
	}
	if len(missing) > 0 {
		t.Fatalf("exemptions name tables bootstrap does not create: %v", missing)
	}
}

// No DB needed: the dropped table's exemption is gone, and the other
// audit-chain exemptions stay (the identity bindings are kept).
func TestRetentionExemptions_PrincipalFilesRemoved(t *testing.T) {
	if why, ok := retentionExemptions["guard_principal_files"]; ok {
		t.Fatalf("guard_principal_files is still exempt (%q) after its table was dropped", why)
	}
	for _, kept := range []string{"guard_identity_bindings", "audit_chain_link",
		"audit_chain_meta", "siem_cursor"} {
		if retentionExemptions[kept] == "" {
			t.Errorf("exemption %s was removed with guard_principal_files", kept)
		}
	}
}
