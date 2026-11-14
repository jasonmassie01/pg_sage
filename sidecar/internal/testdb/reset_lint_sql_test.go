package testdb

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

// lintLiterals returns the string literals under root that are SQL which
// resets or disturbs the whole cluster and that runs (lintRuns).
func lintLiterals(fset *token.FileSet, rel string, root ast.Node) []resetFinding {
	var out []resetFinding
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if what := lintOffence(text); what != "" && lintRuns(stack) {
			out = append(out, resetFinding{file: rel, line: fset.Position(lit.Pos()).Line,
				what: what})
		}
		return true
	})
	return out
}

// lintOffence names what text does to the whole cluster, or "".
func lintOffence(text string) string {
	switch {
	case lintZeroArgReset.MatchString(text), lintAllZeroReset.MatchString(text):
		return "a zero-argument (or all-zero) pg_stat_statements_reset empties every " +
			"database's statements; use testdb.ResetStatements, or testdb.Exclusive"
	case lintResetShared.MatchString(text):
		return "pg_stat_reset_shared resets cluster-wide statistics; " +
			"run it in a test that calls testdb.Exclusive"
	}
	// Upper case only: a lower-case "checkpoint" in tests is a log keyword.
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ";"))
	if trimmed == "CHECKPOINT" {
		return "an executed CHECKPOINT moves the cluster's checkpoint and WAL counters; " +
			"run it in a test that calls testdb.Exclusive"
	}
	return ""
}

// lintRuns reports whether the literal on top of stack is SQL that runs:
// the SQL argument of an Exec/Query-like call, an element of a []string
// list, or any other expression (an assignment, a return, a package
// variable: fail closed). Table data, map values, comparisons, switch
// cases and arguments of other calls are data. Concatenation, parentheses
// and fmt.Sprintf pass through.
func lintRuns(stack []ast.Node) bool {
	i := len(stack) - 1
	unit := stack[i]
	for ; i > 0; i-- {
		parent := stack[i-1]
		if b, ok := parent.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			unit = parent
			continue
		}
		if _, ok := parent.(*ast.ParenExpr); ok {
			unit = parent
			continue
		}
		if call, ok := parent.(*ast.CallExpr); ok && lintIsSprintf(call) {
			unit = parent
			continue
		}
		break
	}
	if i == 0 {
		return true
	}
	switch p := stack[i-1].(type) {
	case *ast.CallExpr:
		return lintSQLCall(p) && lintSQLArg(p) == unit
	case *ast.CompositeLit:
		arr, ok := p.Type.(*ast.ArrayType)
		if !ok {
			return false
		}
		elt, ok := arr.Elt.(*ast.Ident)
		return ok && elt.Name == "string"
	case *ast.KeyValueExpr, *ast.BinaryExpr, *ast.CaseClause:
		return false // a map value or struct field, a comparison, a switch case
	}
	return true
}

func lintIsSprintf(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "fmt" && (sel.Sel.Name == "Sprintf" || sel.Sel.Name == "Sprint")
}

// lintSQLCall reports whether call runs SQL: Exec, Query, QueryRow, a
// batch Queue, or a helper named like them (mustExec, f.exec).
func lintSQLCall(call *ast.CallExpr) bool {
	var name string
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
	case *ast.SelectorExpr:
		name = fun.Sel.Name
	}
	name = strings.ToLower(name)
	return strings.Contains(name, "exec") || strings.Contains(name, "query") ||
		strings.Contains(name, "queue")
}

// lintSQLArg is call's SQL argument: the first argument written as a
// string (a literal, a concatenation or fmt.Sprintf). Later string
// arguments are query parameters, which are data.
func lintSQLArg(call *ast.CallExpr) ast.Node {
	for _, arg := range call.Args {
		if lintIsString(arg) {
			return arg
		}
	}
	return nil
}

func lintIsString(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING
	case *ast.ParenExpr:
		return lintIsString(v.X)
	case *ast.BinaryExpr:
		return v.Op == token.ADD && (lintIsString(v.X) || lintIsString(v.Y))
	case *ast.CallExpr:
		return lintIsSprintf(v)
	}
	return false
}
