package mutationcensus

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

// sqlCallSink reports Rule A for one call: the SQL argument of a driver
// call, or of a package wrapper that forwards its SQL to one, writes
// outside the sage schema. CopyFrom is always a write; it is a sink unless
// its pgx.Identifier names sage.
func sqlCallSink(call *ast.CallExpr, r *resolver, wrappers map[string]int,
	key string) (sink, bool) {
	arg, name, ok := sqlArg(call, wrappers)
	if !ok {
		return sink{}, false
	}
	if name == "CopyFrom" {
		if copyTargetsSage(call) {
			return sink{}, false
		}
		return sink{key: key, rule: "A", detail: "CopyFrom"}, true
	}
	texts, ok := r.texts(arg, 0)
	if !ok {
		return sink{}, false
	}
	for _, text := range texts {
		if w := writeStatements(text); len(w) > 0 {
			return sink{key: key, rule: "A", detail: firstLine(w[0])}, true
		}
	}
	return sink{}, false
}

// sqlArg returns the SQL argument of a call that runs SQL and the callee's
// name. Batch.Queue takes its SQL first. Every other pgx driver method
// takes a context first, so with two or more arguments the SQL is the
// second one unless the first is visibly SQL text; a context of any name
// (c, bg, r.Context()) is read that way.
func sqlArg(call *ast.CallExpr, wrappers map[string]int) (ast.Expr, string, bool) {
	name := calleeName(call)
	if idx, ok := wrappers[name]; ok {
		if idx >= len(call.Args) {
			return nil, "", false
		}
		return call.Args[idx], name, true
	}
	if _, sel := call.Fun.(*ast.SelectorExpr); !sel || !sqlMethods[name] {
		return nil, "", false
	}
	i := 0
	if name != "Queue" && len(call.Args) > 1 && !visiblySQL(call.Args[0]) {
		i = 1
	}
	if i >= len(call.Args) {
		return nil, "", false
	}
	return call.Args[i], name, true
}

func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// visiblySQL reports whether an expression is plainly SQL text: a string
// literal, a concatenation or a fmt call, never a context.
func visiblySQL(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.ParenExpr:
		return visiblySQL(e.X)
	case *ast.BinaryExpr:
		return e.Op == token.ADD
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "fmt"
	}
	return false
}

// sqlWrappers finds the package's functions that forward a string
// parameter as the SQL of a driver call or of another wrapper, mapped to
// that parameter's position. Calls to them are read like driver calls.
func sqlWrappers(files []*ast.File) map[string]int {
	wrappers := map[string]int{}
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if _, known := wrappers[fn.Name.Name]; known {
					continue
				}
				if idx, ok := forwardedParam(fn, wrappers); ok {
					wrappers[fn.Name.Name] = idx
					changed = true
				}
			}
		}
	}
	return wrappers
}

// forwardedParam reports the position of a string parameter that a
// function passes, unchanged, as the SQL argument of a call that runs SQL.
func forwardedParam(fn *ast.FuncDecl, wrappers map[string]int) (int, bool) {
	params := stringParams(fn.Type)
	if len(params) == 0 {
		return 0, false
	}
	idx, found := 0, false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		arg, _, ok := sqlArg(call, wrappers)
		if !ok {
			return true
		}
		for _, name := range concatIdents(arg) {
			if i, isParam := params[name]; isParam {
				idx, found = i, true
				break
			}
		}
		return !found
	})
	return idx, found
}

// concatIdents lists the bare names of an expression and of the operands
// of its + chain: "/* pg_sage */ "+stmt forwards stmt.
func concatIdents(expr ast.Expr) []string {
	switch e := expr.(type) {
	case *ast.Ident:
		return []string{e.Name}
	case *ast.ParenExpr:
		return concatIdents(e.X)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return append(concatIdents(e.X), concatIdents(e.Y)...)
		}
	}
	return nil
}

func stringParams(ft *ast.FuncType) map[string]int {
	params := map[string]int{}
	pos := 0
	for _, field := range ft.Params.List {
		id, isString := field.Type.(*ast.Ident)
		isString = isString && id.Name == "string"
		names := field.Names
		if len(names) == 0 {
			pos++
			continue
		}
		for _, name := range names {
			if isString {
				params[name.Name] = pos
			}
			pos++
		}
	}
	return params
}

func copyTargetsSage(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.CompositeLit)
		if !ok || len(lit.Elts) < 2 {
			continue
		}
		if b, ok := lit.Elts[0].(*ast.BasicLit); ok {
			schema, err := strconv.Unquote(b.Value)
			return err == nil && schema == "sage"
		}
	}
	return false
}

func firstLine(stmt string) string {
	stmt = strings.Join(strings.Fields(stmt), " ")
	if len(stmt) > 60 {
		stmt = stmt[:60]
	}
	return stmt
}
