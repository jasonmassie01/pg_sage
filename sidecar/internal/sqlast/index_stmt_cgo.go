//go:build cgo

package sqlast

import (
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// checkIndexStmt refuses a CREATE INDEX whose table is unqualified (it
// would resolve through the session search_path at run time), and one
// whose key expressions or predicate call a function qualified by a
// protected schema other than pg_catalog. A bare call is accepted: the
// executor's pinned search_path binds it to pg_catalog, and whether a call
// into any other schema binds an extension's member is decided against the
// catalog before the build.
func checkIndexStmt(idx *pg_query.IndexStmt, rules Rules) error {
	if idx.GetRelation().GetSchemaname() == "" {
		return reject("CREATE INDEX target must be schema-qualified")
	}
	var refused error
	walk(idx.ProtoReflect(), func(msg protoreflect.Message) bool {
		call, ok := msg.Interface().(*pg_query.FuncCall)
		if !ok {
			return true
		}
		name := nameFromList(call.GetFuncname())
		schema := strings.ToLower(name.Schema)
		if schema != "" && schema != "pg_catalog" && rules.ProtectedSchema != nil &&
			rules.ProtectedSchema(schema) {
			refused = reject("function %s is in protected schema %q", name, schema)
			return false
		}
		return true
	})
	return refused
}
