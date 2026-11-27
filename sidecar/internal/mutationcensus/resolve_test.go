package mutationcensus

import (
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// placeholder stands for any part of a SQL string the scan cannot read.
const placeholder = "?"

// maxCandidates bounds the strings one expression can resolve to.
const maxCandidates = 32

// printfVerb is one fmt verb, with its optional explicit argument index.
var printfVerb = regexp.MustCompile(`%(?:\[(\d+)\])?[-+# 0]*\d*(?:\.\d+)?[a-zA-Z%]`)

// resolver turns an expression into the SQL strings it can hold.
type resolver struct {
	pkg, local map[string][]ast.Expr
	// funcs are the package's functions and methods by name, so a call to
	// a local SQL builder reads as the strings it returns.
	funcs map[string][]*ast.FuncDecl
}

// localValues collects every assignment to a name inside one function,
// and the elements a range over a literal slice gives its value.
func localValues(body ast.Node) map[string][]ast.Expr {
	values := map[string][]ast.Expr{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != len(node.Rhs) {
				return true
			}
			for i, lhs := range node.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					values[id.Name] = append(values[id.Name], node.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			if len(node.Names) != len(node.Values) {
				return true
			}
			for i, id := range node.Names {
				values[id.Name] = append(values[id.Name], node.Values[i])
			}
		case *ast.RangeStmt:
			id, isIdent := node.Value.(*ast.Ident)
			lit, isLit := node.X.(*ast.CompositeLit)
			if isIdent && isLit {
				values[id.Name] = append(values[id.Name], lit.Elts...)
			}
		}
		return true
	})
	return values
}

// texts returns the strings an expression can hold and whether any part
// of it was a string the scan could read.
func (r *resolver) texts(expr ast.Expr, depth int) ([]string, bool) {
	if depth > 8 {
		return []string{placeholder}, false
	}
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return []string{placeholder}, false
		}
		text, err := strconv.Unquote(e.Value)
		if err != nil {
			return []string{placeholder}, false
		}
		return []string{text}, true
	case *ast.ParenExpr:
		return r.texts(e.X, depth+1)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return []string{placeholder}, false
		}
		left, lok := r.texts(e.X, depth+1)
		right, rok := r.texts(e.Y, depth+1)
		return product(left, right), lok || rok
	case *ast.Ident:
		return r.ident(e.Name, depth)
	case *ast.CallExpr:
		return r.call(e, depth)
	}
	return []string{placeholder}, false
}

func (r *resolver) ident(name string, depth int) ([]string, bool) {
	exprs := r.local[name]
	if len(exprs) == 0 {
		exprs = r.pkg[name]
	}
	var out []string
	resolved := false
	for _, expr := range exprs {
		texts, ok := r.texts(expr, depth+1)
		resolved = resolved || ok
		out = appendCapped(out, texts...)
	}
	if len(out) == 0 {
		return []string{placeholder}, false
	}
	return out, resolved
}

// call reads fmt.Sprintf with its arguments substituted, the input of
// strings.Trim*/Replace* wrappers, pgx.Identifier{...}.Sanitize() and the
// returns of the package's own functions; any other call reads as its
// literal arguments.
func (r *resolver) call(call *ast.CallExpr, depth int) ([]string, bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if isSel && sel.Sel.Name == "Sanitize" {
		if lit, ok := sel.X.(*ast.CompositeLit); ok {
			return r.sanitized(lit, depth), true
		}
	}
	if pkg, ok := selectorPkg(sel, isSel); ok && len(call.Args) > 0 {
		switch {
		case pkg == "fmt" && sel.Sel.Name == "Sprintf":
			return r.sprintf(call, depth)
		case pkg == "fmt" && sel.Sel.Name == "Sprint",
			pkg == "strings" && strings.HasPrefix(sel.Sel.Name, "Trim"),
			pkg == "strings" && strings.HasPrefix(sel.Sel.Name, "Replace"):
			return r.texts(call.Args[0], depth+1)
		}
		return literalArgs(call), false
	}
	for _, fn := range r.funcs[calleeName(call)] {
		if (fn.Recv != nil) != isSel || fn.Body == nil {
			continue
		}
		if texts, ok := r.returns(fn, call, depth); ok {
			return texts, true
		}
	}
	return literalArgs(call), false
}

func selectorPkg(sel *ast.SelectorExpr, isSel bool) (string, bool) {
	if !isSel {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || (id.Name != "fmt" && id.Name != "strings") {
		return "", false
	}
	return id.Name, true
}

// sanitized renders pgx.Identifier{"sage", name}.Sanitize() as "sage".?:
// each readable part quoted, the rest the placeholder.
func (r *resolver) sanitized(lit *ast.CompositeLit, depth int) []string {
	out := []string{""}
	for i, elt := range lit.Elts {
		if i > 0 {
			out = product(out, []string{"."})
		}
		texts, ok := r.texts(elt, depth+1)
		if !ok {
			out = product(out, []string{placeholder})
			continue
		}
		quoted := make([]string, len(texts))
		for j, text := range texts {
			quoted[j] = `"` + text + `"`
		}
		out = product(out, quoted)
	}
	return out
}

// sprintf substitutes each verb of the format with its argument's texts.
func (r *resolver) sprintf(call *ast.CallExpr, depth int) ([]string, bool) {
	formats, ok := r.texts(call.Args[0], depth+1)
	var out []string
	for _, format := range formats {
		out = appendCapped(out, r.substitute(format, call.Args[1:], depth)...)
	}
	return out, ok
}

func (r *resolver) substitute(format string, args []ast.Expr, depth int) []string {
	out := []string{""}
	next, last := 0, 0
	for _, loc := range printfVerb.FindAllStringSubmatchIndex(format, -1) {
		out = product(out, []string{format[last:loc[0]]})
		last = loc[1]
		if format[loc[1]-1] == '%' {
			out = product(out, []string{"%"})
			continue
		}
		if loc[2] >= 0 {
			if n, err := strconv.Atoi(format[loc[2]:loc[3]]); err == nil {
				next = n - 1
			}
		}
		texts := []string{placeholder}
		if next >= 0 && next < len(args) {
			texts, _ = r.texts(args[next], depth+1)
		}
		next++
		out = product(out, texts)
	}
	return product(out, []string{format[last:]})
}

// returns reads the first result of every return statement of fn, with
// its string parameters bound to what the call passes.
func (r *resolver) returns(fn *ast.FuncDecl, call *ast.CallExpr, depth int) ([]string, bool) {
	callee := &resolver{pkg: r.pkg, local: localValues(fn.Body), funcs: r.funcs}
	for name, pos := range stringParams(fn.Type) {
		if pos >= len(call.Args) {
			continue
		}
		if texts, ok := r.texts(call.Args[pos], depth+1); ok {
			callee.local[name] = append(callee.local[name], literals(texts)...)
		}
	}
	var out []string
	resolved := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		texts, ok := callee.texts(ret.Results[0], depth+1)
		resolved = resolved || ok
		out = appendCapped(out, texts...)
		return true
	})
	return out, resolved
}

// literals turns resolved texts back into expressions a resolver reads.
func literals(texts []string) []ast.Expr {
	out := make([]ast.Expr, len(texts))
	for i, text := range texts {
		out[i] = &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(text)}
	}
	return out
}

// literalArgs renders a call the scan cannot evaluate as its string literal
// arguments (a quoting helper returns its input, so quote("sage") reads as
// sage), or as the placeholder when it has none.
func literalArgs(call *ast.CallExpr) []string {
	var parts []string
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if text, err := strconv.Unquote(lit.Value); err == nil {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return []string{placeholder}
	}
	return []string{strings.Join(parts, "")}
}

func product(left, right []string) []string {
	var out []string
	for _, l := range left {
		for _, rt := range right {
			out = appendCapped(out, l+rt)
		}
	}
	return out
}

func appendCapped(out []string, texts ...string) []string {
	for _, t := range texts {
		if len(out) >= maxCandidates {
			return out
		}
		out = append(out, t)
	}
	return out
}
