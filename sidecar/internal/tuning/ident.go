package tuning

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Identifiers: catalog names are folded like PostgreSQL folds them (the
// reader is workload.ReadQualified, shared with the optimizer), and
// rendered by sqlident, quoted only when they must be (upper case, special
// characters or a key word that the server's quote_ident quotes), so
// "public.orders" and `"Sales"."Orders"` are the canonical forms of the
// tables the agent names.

// splitQualified parses a whole "schema.name" reference; both parts are
// required and nothing may follow.
func splitQualified(ref string) (schema, name string, ok bool) {
	s := strings.TrimSpace(ref)
	schema, name, next, err := workload.ReadQualified(s, 0)
	if err != nil || schema == "" || next != len(s) {
		return "", "", false
	}
	return schema, name, true
}

// canonicalRef is ref in canonical form, or "" when it is not a qualified
// name.
func canonicalRef(ref string) string {
	schema, name, ok := splitQualified(ref)
	if !ok {
		return ""
	}
	return sqlident.Qualified(schema, name)
}
