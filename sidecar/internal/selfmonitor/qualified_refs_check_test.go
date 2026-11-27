package selfmonitor

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// security-01c: pg_sage's observation sessions are to run under
// search_path = pg_catalog, pg_temp, where an extension object (in public
// or any other schema) resolves only when its schema is named. This
// test-only check lists every SQL literal in production code that names
// one without a schema, by file and line. pg_catalog names need none:
// pg_catalog is first on the pinned path and no other schema is searched
// for functions. It reads Go syntax trees and the tagger's own SQL lexer
// (tokenLen), so it runs without cgo, where sqlast cannot parse.

// extensionObjects are the extension relations and functions pg_sage
// reads; names starting with hypopg are HypoPG's.
var extensionObjects = map[string]bool{
	"pg_stat_statements": true, "pg_stat_statements_info": true,
	"pg_stat_statements_reset": true, "pgstattuple": true, "pgstattuple_approx": true,
	"pgstatindex": true, "pgstatginindex": true, "pgstathashindex": true,
	"pg_relpages": true, "pg_buffercache": true, "pg_buffercache_summary": true,
	"pg_buffercache_usage_counts": true, "pg_visibility_map": true, "pg_visibility": true,
}

func isExtensionObject(name string) bool {
	name = strings.ToLower(name)
	return extensionObjects[name] || name == "hypopg" || strings.HasPrefix(name, "hypopg_")
}

// sqlTokens is the significant tokens of sql (no white space or
// comments), each with its byte offset. An unreadable unit (tokenLen -1)
// is skipped: a literal holding a backslash as far as its closing quote
// (standard_conforming_strings), one cut by string concatenation to the
// end, anything else a byte at a time.
func sqlTokens(sql string) (toks []string, offs []int) {
	for i := 0; i < len(sql); {
		n := tokenLen(sql[i:])
		switch {
		case n < 0 && sql[i] == '\'':
			n = conformingLiteralLen(sql[i:])
		case n < 0:
			n = 1
		default:
			if tok := sql[i : i+n]; !isSpaceOrComment(tok) {
				toks, offs = append(toks, tok), append(offs, i)
			}
		}
		i += n
	}
	return toks, offs
}

// conformingLiteralLen is the length of the literal s starts with when a
// backslash is an ordinary character, or len(s) when it is unterminated.
func conformingLiteralLen(s string) int {
	for j := 1; j < len(s); j++ {
		if s[j] != '\'' {
			continue
		}
		if j+1 < len(s) && s[j+1] == '\'' {
			j++
			continue
		}
		return j + 1
	}
	return len(s)
}

func isSpaceOrComment(tok string) bool {
	return strings.TrimSpace(tok) == "" || strings.HasPrefix(tok, "--") ||
		strings.HasPrefix(tok, "/*")
}

// looksLikeSQL reports a statement keyword among the tokens; error
// messages and plain names have none.
func looksLikeSQL(toks []string) bool {
	for _, tok := range toks {
		switch strings.ToLower(tok) {
		case "select", "explain", "join":
			return true
		}
	}
	return false
}

// unqualifiedRefs returns the offsets and names of the extension objects
// the tokens name without a schema: an identifier next to no '.' (a
// setting such as pg_stat_statements.max is next to one) that does not
// follow EXTENSION, or the literal of an OID lookup (to_regclass('x')).
func unqualifiedRefs(toks []string, offs []int) (at []int, names []string) {
	tok := func(i int) string {
		if i < 0 || i >= len(toks) {
			return ""
		}
		return strings.ToLower(toks[i])
	}
	for i, t := range toks {
		name := t
		if strings.HasPrefix(t, "'") {
			name = regLiteralName(t, tok(i-1), tok(i-2), tok(i+1)+tok(i+2)+tok(i+3))
		} else if !isIdentStart(t[0]) || tok(i-1) == "." || tok(i+1) == "." ||
			afterExtensionKeyword(tok, i) {
			continue
		}
		if name != "" && isExtensionObject(name) {
			at, names = append(at, offs[i]), append(names, strings.ToLower(name))
		}
	}
	return at, names
}

// regLiteralName is the object a literal names for an OID lookup
// (to_regclass('x'), 'x'::regclass, to_regprocedure('f(text)')), or ""
// for any other literal or a qualified name.
func regLiteralName(lit, prev, prev2, next string) string {
	reg := prev == "(" && strings.HasPrefix(prev2, "to_reg") ||
		strings.HasPrefix(next, "::reg")
	value := strings.Trim(lit, "'")
	if !reg || strings.Contains(value, ".") {
		return ""
	}
	name, _, _ := strings.Cut(value, "(")
	return strings.TrimSpace(name)
}

// afterExtensionKeyword reports EXTENSION among the four tokens before
// i: CREATE EXTENSION IF NOT EXISTS hypopg names the extension itself.
func afterExtensionKeyword(tok func(int) string, i int) bool {
	for j := i - 1; j >= i-4 && j >= 0; j-- {
		if tok(j) == "extension" {
			return true
		}
	}
	return false
}

type unqualifiedRef struct {
	file string
	line int
	name string
}

func (r unqualifiedRef) String() string {
	return fmt.Sprintf("%s:%d: %s", r.file, r.line, r.name)
}

// proseMarker on the line of a chain's first literal, or the line above
// it, marks the chain as model prose, not SQL: an LLM prompt or a tool
// description that names an extension in words. A reason must follow.
const proseMarker = "//qualifiedrefs:prose "

// proseLines returns the lines holding a prose marker with a reason.
func proseLines(fset *token.FileSet, f *ast.File) map[int]bool {
	lines := map[int]bool{}
	for _, group := range f.Comments {
		for _, c := range group.List {
			reason, ok := strings.CutPrefix(c.Text, proseMarker)
			if ok && strings.TrimSpace(reason) != "" {
				lines[fset.Position(c.Slash).Line] = true
			}
		}
	}
	return lines
}

// startsAsStatement reports a chain whose text opens with a statement
// keyword: SQL, which a prose marker cannot excuse.
func startsAsStatement(toks []string) bool {
	if len(toks) == 0 {
		return false
	}
	switch strings.ToLower(toks[0]) {
	case "select", "with", "explain", "insert", "update", "delete", "values", "table":
		return true
	}
	return false
}

// fileRefs parses one Go source and checks each string concatenation
// (or lone literal) that looks like SQL and is not marked as prose.
func fileRefs(fset *token.FileSet, name string, src any) ([]unqualifiedRef, error) {
	f, err := parser.ParseFile(fset, name, src,
		parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		return nil, err
	}
	prose := proseLines(fset, f)
	var out []unqualifiedRef
	seen := map[*ast.BasicLit]bool{} // a chain's inner chains are its own
	ast.Inspect(f, func(n ast.Node) bool {
		lits, ok := literalChain(n)
		if !ok || seen[lits[0]] {
			return true
		}
		for _, lit := range lits {
			seen[lit] = true
		}
		var all []string
		for _, lit := range lits {
			toks, _ := sqlTokens(literalValue(lit))
			all = append(all, toks...)
		}
		line := fset.Position(lits[0].Pos()).Line
		if (prose[line] || prose[line-1]) && !startsAsStatement(all) {
			return true
		}
		if looksLikeSQL(all) {
			for _, lit := range lits {
				out = append(out, literalRefs(fset, lit)...)
			}
		}
		return true // a call inside the chain may hold a statement of its own
	})
	return out, nil
}

// literalChain returns the string literals of n when n is a string
// literal or a + chain holding at least one; ok is false otherwise. Only
// the chain's own operands count: a literal passed to a call inside it
// (pgx.Identifier{schema, "hypopg_reset"}) is an argument, not SQL text,
// and the walk reaches it on its own.
func literalChain(n ast.Node) ([]*ast.BasicLit, bool) {
	switch e := n.(type) {
	case *ast.BasicLit:
		return []*ast.BasicLit{e}, e.Kind == token.STRING
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return nil, false
		}
		lits := chainOperands(e)
		return lits, len(lits) > 0
	}
	return nil, false
}

func chainOperands(e ast.Expr) []*ast.BasicLit {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			return []*ast.BasicLit{x}
		}
	case *ast.ParenExpr:
		return chainOperands(x.X)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return append(chainOperands(x.X), chainOperands(x.Y)...)
		}
	}
	return nil
}

func literalValue(lit *ast.BasicLit) string {
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return v
}

// literalRefs reports lit's references; a raw string's line is the
// reference's own, an interpreted one's the literal's.
func literalRefs(fset *token.FileSet, lit *ast.BasicLit) []unqualifiedRef {
	value := literalValue(lit)
	pos := fset.Position(lit.Pos())
	at, names := unqualifiedRefs(sqlTokens(value))
	out := make([]unqualifiedRef, 0, len(at))
	for k, off := range at {
		line := pos.Line
		if strings.HasPrefix(lit.Value, "`") {
			line += strings.Count(value[:off], "\n")
		}
		out = append(out, unqualifiedRef{file: filepath.ToSlash(pos.Filename),
			line: line, name: names[k]})
	}
	return out
}

// skipDir reports the trees the check leaves out: test helpers and
// fixtures, which never run on an observation session.
func skipDir(name string) bool {
	return name == "testdata" || name == "testsupport" || name == "testdb"
}

// productionRefs checks every non-test Go file under the module's
// internal and cmd trees.
func productionRefs(t *testing.T) []unqualifiedRef {
	t.Helper()
	refs, err := treeRefs(filepath.Join("..", ".."), "internal", "cmd")
	if err != nil {
		t.Fatalf("scan production code: %v", err)
	}
	return refs
}

// treeRefs checks every non-test Go file under root's dirs, naming each
// file relative to root.
func treeRefs(root string, dirs ...string) ([]unqualifiedRef, error) {
	fset := token.NewFileSet()
	var out []unqualifiedRef
	for _, dir := range dirs {
		walk := func(path string, d os.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && skipDir(d.Name()):
				return filepath.SkipDir
			case d.IsDir(), !strings.HasSuffix(path, ".go"),
				strings.HasSuffix(path, "_test.go"):
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			refs, err := fileRefs(fset, filepath.ToSlash(rel), src)
			out = append(out, refs...)
			return err
		}
		if err := filepath.WalkDir(filepath.Join(root, dir), walk); err != nil {
			return nil, fmt.Errorf("scan %s: %w", dir, err)
		}
	}
	return out, nil
}
