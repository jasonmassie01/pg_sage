//go:build cgo

package sqlast

import (
	"sort"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// InspectBrokeredRead accepts exactly one plain SELECT (WITH, VALUES and
// TABLE forms included) with no data-modifying statement, no locking
// clause and no INTO, and returns its canonical form. The canonical form
// must fingerprint like the original (deparse fidelity), or the statement
// is rejected: a statement the deparser would change is not run.
func InspectBrokeredRead(sql string) (BrokeredRead, error) {
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return BrokeredRead{}, reject("parse error: %v", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return BrokeredRead{}, reject("exactly one statement is allowed, got %d", n)
	}
	if tree.GetStmts()[0].GetStmt().GetSelectStmt() == nil {
		return BrokeredRead{}, reject("only a SELECT statement is allowed")
	}
	canonical, err := pg_query.Deparse(tree)
	if err != nil {
		return BrokeredRead{}, reject("cannot deparse the statement: %v", err)
	}
	if err := sameFingerprint(sql, canonical); err != nil {
		return BrokeredRead{}, err
	}
	q, err := InspectReadQuery(canonical)
	if err != nil {
		return BrokeredRead{}, err
	}
	switch {
	case q.ModifiesData:
		return BrokeredRead{}, reject("a data-modifying statement is not a read")
	case q.LockingClause:
		return BrokeredRead{}, reject("a locking clause (FOR UPDATE/SHARE) is not a read")
	case q.SelectInto:
		return BrokeredRead{}, reject("SELECT INTO creates a table")
	}
	return describeBrokered(canonical, q)
}

func sameFingerprint(original, canonical string) error {
	before, err := pg_query.Fingerprint(original)
	if err != nil {
		return reject("cannot fingerprint the statement: %v", err)
	}
	after, err := pg_query.Fingerprint(canonical)
	if err != nil || after != before {
		return reject("the statement does not survive deparsing unchanged")
	}
	return nil
}

// describeBrokered re-parses the canonical text and lists its parameters
// and column references.
func describeBrokered(canonical string, q ReadQuery) (BrokeredRead, error) {
	tree, err := pg_query.Parse(canonical)
	if err != nil {
		return BrokeredRead{}, reject("parse error in the canonical form: %v", err)
	}
	root := tree.GetStmts()[0].GetStmt()
	fp, err := pg_query.Fingerprint(canonical)
	if err != nil {
		return BrokeredRead{}, reject("cannot fingerprint the statement: %v", err)
	}
	out := BrokeredRead{Canonical: canonical, Fingerprint: fp, Query: q}
	total := map[string]int{}
	walk(root.ProtoReflect(), func(msg protoreflect.Message) bool {
		switch n := msg.Interface().(type) {
		case *pg_query.ParamRef:
			out.Params = max(out.Params, int(n.GetNumber()))
		case *pg_query.ColumnRef:
			if name := lastFieldName(n); name != "" {
				total[name]++
			}
		}
		return true
	})
	bare := bareOutputColumns(root.GetSelectStmt())
	other := map[string]bool{}
	for name, count := range total {
		if count > bare[name] {
			other[name] = true
		}
	}
	out.OutputColumns, out.OtherColumnRefs = sortedNames(bare), sortedNames(other)
	return out, nil
}

// bareOutputColumns counts the column references that are whole entries of
// the top-level target list. A set operation has no such entries: its
// outputs are computed.
func bareOutputColumns(sel *pg_query.SelectStmt) map[string]int {
	out := map[string]int{}
	if sel == nil || sel.GetOp() != pg_query.SetOperation_SETOP_NONE {
		return out
	}
	for _, item := range sel.GetTargetList() {
		ref := item.GetResTarget().GetVal().GetColumnRef()
		if name := lastFieldName(ref); name != "" {
			out[name]++
		}
	}
	return out
}

// lastFieldName is the referenced name of a ColumnRef ("" for * forms).
func lastFieldName(ref *pg_query.ColumnRef) string {
	fields := ref.GetFields()
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1].GetString_().GetSval()
}

func sortedNames[V int | bool](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
