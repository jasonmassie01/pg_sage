//go:build cgo

package sqlast

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"github.com/pganalyze/pg_query_go/v6/parser"
)

// parseCalls counts libpg_query invocations by the change-review
// inspections, so tests can prove oversized input never reaches the parser.
var parseCalls atomic.Int64

// checkSize rejects inputs over MaxInputBytes before any parsing.
func checkSize(sql string) error {
	if len(sql) > MaxInputBytes {
		return fmt.Errorf("%w: %w: input is %d bytes, the limit is %d",
			ErrRejected, ErrInputTooLarge, len(sql), MaxInputBytes)
	}
	return nil
}

// parseScript parses a whole script, mapping parser failures to *ParseError.
func parseScript(sql string) (*pg_query.ParseResult, error) {
	if err := checkSize(sql); err != nil {
		return nil, err
	}
	parseCalls.Add(1)
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return nil, parseError(sql, err)
	}
	return tree, nil
}

// parseOne parses exactly one statement.
func parseOne(sql string) (*pg_query.ParseResult, *pg_query.Node, error) {
	tree, err := parseScript(sql)
	if err != nil {
		return nil, nil, err
	}
	switch n := len(tree.GetStmts()); n {
	case 0:
		return nil, nil, reject("no statement")
	case 1:
		return tree, tree.GetStmts()[0].GetStmt(), nil
	default:
		return nil, nil, reject("exactly one statement is allowed, got %d", n)
	}
}

// parseError converts libpg_query's error, whose cursor is a 1-based
// character position, into a byte offset and line.
func parseError(sql string, err error) error {
	var pe *parser.Error
	if !errors.As(err, &pe) {
		return &ParseError{Message: err.Error(), Offset: -1}
	}
	out := &ParseError{Message: pe.Message, Offset: -1}
	if pe.Cursorpos > 0 {
		out.Offset = byteOffset(sql, pe.Cursorpos-1)
		out.Line = 1 + strings.Count(sql[:out.Offset], "\n")
	}
	return out
}

// byteOffset returns the byte offset of the chars-th character, clamped to
// the input length.
func byteOffset(sql string, chars int) int {
	offset := 0
	for i := 0; i < chars && offset < len(sql); i++ {
		_, size := utf8.DecodeRuneInString(sql[offset:])
		offset += size
	}
	return offset
}

// nodeName is the parse-tree message name of a statement node, e.g.
// "AlterTableStmt".
func nodeName(node *pg_query.Node) string {
	msg := node.ProtoReflect()
	field := msg.WhichOneof(msg.Descriptor().Oneofs().ByName("node"))
	if field == nil {
		return ""
	}
	return string(field.Message().Name())
}

// deparseExpr renders an expression through libpg_query's deparser by
// wrapping it in a SELECT target list.
func deparseExpr(expr *pg_query.Node) string {
	if expr == nil {
		return ""
	}
	target := &pg_query.ResTarget{Val: expr}
	sel := &pg_query.SelectStmt{TargetList: []*pg_query.Node{
		{Node: &pg_query.Node_ResTarget{ResTarget: target}}}}
	out, err := deparseSelect(sel)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(out, "SELECT ")
}

// deparseSelect renders one SELECT statement.
func deparseSelect(sel *pg_query.SelectStmt) (string, error) {
	tree := &pg_query.ParseResult{Stmts: []*pg_query.RawStmt{{Stmt: &pg_query.Node{
		Node: &pg_query.Node_SelectStmt{SelectStmt: sel}}}}}
	out, err := pg_query.Deparse(tree)
	if err != nil {
		return "", fmt.Errorf("deparse: %w", err)
	}
	return out, nil
}

// rangeVarName is a RangeVar's schema and relation name.
func rangeVarName(rv *pg_query.RangeVar) QualifiedName {
	return QualifiedName{Schema: rv.GetSchemaname(), Name: rv.GetRelname()}
}

// stringList returns the String items of a parser list (column lists,
// name lists).
func stringList(items []*pg_query.Node) []string {
	var out []string
	for _, item := range items {
		if s := item.GetString_(); s != nil {
			out = append(out, s.GetSval())
		}
	}
	return out
}
