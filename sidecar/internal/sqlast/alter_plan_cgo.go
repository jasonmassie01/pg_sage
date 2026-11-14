//go:build cgo

package sqlast

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"github.com/pganalyze/pg_query_go/v6/parser"
)

// ParsePlannableAlter reads the one ALTER TABLE clause the online-migration
// planner rewrites: ADD CONSTRAINT name UNIQUE (col, ...) with optional
// constraint timing, or ALTER [COLUMN] col SET NOT NULL. The parse tree
// decides what the SQL says; anything the rewritten plan could not carry
// verbatim is refused with a named error, never dropped. Every result has
// passed the scanner check on identifier length, which the parser's own
// silent truncation to 63 bytes cannot hide.
func ParsePlannableAlter(sql string) (PlannableAlter, error) {
	scanned, err := pg_query.Scan(sql)
	if err != nil {
		return PlannableAlter{}, syntaxError(sql, err)
	}
	tokens := significantTokens(scanned.GetTokens())
	cmd, relation, err := singleAlterCommand(sql)
	if err != nil {
		return PlannableAlter{}, err
	}
	alter := PlannableAlter{Schema: relation.GetSchemaname(), Table: relation.GetRelname()}
	switch cmd.GetSubtype() {
	case pg_query.AlterTableType_AT_SetNotNull:
		alter.Kind, alter.Column = AlterSetNotNull, cmd.GetName()
	case pg_query.AlterTableType_AT_AddConstraint:
		err = readUnique(sql, tokens, cmd.GetDef().GetConstraint(), &alter)
	default:
		err = fmt.Errorf("%w: the planner rewrites ADD CONSTRAINT ... UNIQUE and "+
			"ALTER COLUMN ... SET NOT NULL, not %s; plan it by hand",
			ErrNotPlannable, strings.TrimPrefix(cmd.GetSubtype().String(), "AT_"))
	}
	if err != nil {
		return PlannableAlter{}, err
	}
	if err := checkIdentifiers(sql, tokens); err != nil {
		return PlannableAlter{}, err
	}
	return alter, nil
}

// singleAlterCommand requires exactly one ALTER TABLE statement with one
// command, on a table not qualified by a database name.
func singleAlterCommand(sql string) (*pg_query.AlterTableCmd, *pg_query.RangeVar, error) {
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return nil, nil, syntaxError(sql, err)
	}
	stmts := tree.GetStmts()
	if len(stmts) != 1 {
		return nil, nil, fmt.Errorf("%w: expected one ALTER TABLE statement, found %d",
			ErrNotPlannable, len(stmts))
	}
	alter := stmts[0].GetStmt().GetAlterTableStmt()
	if alter == nil {
		return nil, nil, fmt.Errorf("%w: the planner rewrites ALTER TABLE only, not %s",
			ErrNotPlannable, nodeName(stmts[0].GetStmt()))
	}
	if alter.GetObjtype() != pg_query.ObjectType_OBJECT_TABLE {
		return nil, nil, fmt.Errorf("%w: the planner rewrites ALTER TABLE only, not "+
			"ALTER %s", ErrNotPlannable,
			strings.TrimPrefix(alter.GetObjtype().String(), "OBJECT_"))
	}
	if alter.GetRelation().GetCatalogname() != "" {
		return nil, nil, fmt.Errorf("%w: the target names a database (%s); name the "+
			"table as schema.table", ErrNotPlannable, alter.GetRelation().GetCatalogname())
	}
	cmds := alter.GetCmds()
	if len(cmds) != 1 || cmds[0].GetAlterTableCmd() == nil {
		return nil, nil, fmt.Errorf("%w: the planner handles a single ALTER TABLE "+
			"clause; split the migration", ErrNotPlannable)
	}
	return cmds[0].GetAlterTableCmd(), alter.GetRelation(), nil
}

// significantTokens drops comments and trailing semicolons: a comment is
// never a clause, and the statement count is the parser's to judge.
func significantTokens(all []*pg_query.ScanToken) []*pg_query.ScanToken {
	out := make([]*pg_query.ScanToken, 0, len(all))
	for _, tok := range all {
		switch tok.GetToken() {
		case pg_query.Token_SQL_COMMENT, pg_query.Token_C_COMMENT:
			continue
		}
		out = append(out, tok)
	}
	for len(out) > 0 && out[len(out)-1].GetToken() == pg_query.Token_ASCII_59 {
		out = out[:len(out)-1]
	}
	return out
}

// syntaxError reports SQL PostgreSQL's scanner or parser rejects, with
// the text from where it stopped reading.
func syntaxError(sql string, err error) error {
	var pgErr *parser.Error
	if errors.As(err, &pgErr) && pgErr.Cursorpos > 0 {
		if where := echo(sql[charOffset(sql, pgErr.Cursorpos-1):]); where != "" {
			return fmt.Errorf("%w: %s at %q; plan it by hand", ErrSyntax, pgErr.Message, where)
		}
	}
	return fmt.Errorf("%w: %v; plan it by hand", ErrSyntax, err)
}

// charOffset is the byte offset of the n-th character (0-based) of sql;
// PostgreSQL reports error positions in characters.
func charOffset(sql string, n int) int {
	offset := 0
	for i := 0; i < n && offset < len(sql); i++ {
		_, size := utf8.DecodeRuneInString(sql[offset:])
		offset += size
	}
	return offset
}

// echo trims text for an error message and bounds it at maxEcho bytes,
// cut on a character boundary.
func echo(text string) string {
	text = strings.TrimRight(strings.TrimSpace(text), "; \t\r\n")
	if len(text) <= maxEcho {
		return text
	}
	cut := maxEcho
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

// tokenText is the source text of tok; false when its offsets do not lie
// inside sql.
func tokenText(sql string, tok *pg_query.ScanToken) (string, bool) {
	start, end := int(tok.GetStart()), int(tok.GetEnd())
	if start < 0 || end < start || end > len(sql) {
		return "", false
	}
	return sql[start:end], true
}

// textFrom is sql from tok to the end, the text a refusal repeats.
func textFrom(sql string, tok *pg_query.ScanToken) string {
	start := int(tok.GetStart())
	if start < 0 || start > len(sql) {
		return ""
	}
	return sql[start:]
}
