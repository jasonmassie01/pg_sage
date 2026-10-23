//go:build cgo

package sqlast

import (
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// InspectDDL parses a whole migration script and types every statement.
// A parse error anywhere rejects the script with a *ParseError; there is
// never a partial result. A script without statements yields none.
func InspectDDL(sql string) ([]DDLStatement, error) {
	tree, err := parseScript(sql)
	if err != nil {
		return nil, err
	}
	if len(tree.GetStmts()) == 0 {
		return nil, nil
	}
	tokens, err := scriptTokens(sql)
	if err != nil {
		return nil, err
	}
	state := newScriptState()
	var out []DDLStatement
	for i, raw := range tree.GetStmts() {
		offset, text := statementSpan(sql, tokens, raw)
		line := 1 + strings.Count(sql[:offset], "\n")
		stmt := raw.GetStmt()
		parts := inspectStatement(stmt)
		for p := range parts {
			parts[p].Command = nodeName(stmt)
			parts[p].Statement, parts[p].Part = i, p
			parts[p].Offset, parts[p].Line, parts[p].Text = offset, line, text
			state.annotate(&parts[p])
		}
		state.apply(stmt)
		out = append(out, parts...)
	}
	return out, nil
}

// tokenSpan is the byte range of one significant token: comments and
// statement-separating semicolons are not significant.
type tokenSpan struct{ start, end int }

func scriptTokens(sql string) ([]tokenSpan, error) {
	parseCalls.Add(1)
	scan, err := pg_query.Scan(sql)
	if err != nil {
		return nil, parseError(sql, err)
	}
	tokens := make([]tokenSpan, 0, len(scan.GetTokens()))
	for _, tok := range scan.GetTokens() {
		switch tok.GetToken() {
		case pg_query.Token_SQL_COMMENT, pg_query.Token_C_COMMENT, pg_query.Token_ASCII_59:
			continue
		}
		tokens = append(tokens, tokenSpan{int(tok.GetStart()), int(tok.GetEnd())})
	}
	return tokens, nil
}

// statementSpan returns the offset of the statement's first token and its
// text up to the end of its last token, so surrounding whitespace, comments
// and the semicolon are dropped.
func statementSpan(sql string, tokens []tokenSpan, raw *pg_query.RawStmt) (int, string) {
	start := int(raw.GetStmtLocation())
	end := len(sql)
	if raw.GetStmtLen() > 0 {
		end = start + int(raw.GetStmtLen())
	}
	first := sort.Search(len(tokens), func(i int) bool { return tokens[i].start >= start })
	last := first
	for last+1 < len(tokens) && tokens[last+1].end <= end {
		last++
	}
	if first >= len(tokens) || tokens[first].start >= end {
		text := strings.TrimSpace(sql[start:end])
		return start + strings.Index(sql[start:end], text), text
	}
	return tokens[first].start, sql[tokens[first].start:tokens[last].end]
}

func inspectStatement(node *pg_query.Node) []DDLStatement {
	switch {
	case node.GetIndexStmt() != nil:
		return []DDLStatement{createIndex(node.GetIndexStmt())}
	case node.GetDropStmt() != nil:
		return dropStatements(node.GetDropStmt())
	case node.GetCreateStmt() != nil:
		return []DDLStatement{createTable(node.GetCreateStmt())}
	case node.GetRenameStmt() != nil:
		return []DDLStatement{renameStatement(node.GetRenameStmt())}
	case node.GetAlterTableStmt() != nil:
		return alterTable(node.GetAlterTableStmt())
	case node.GetCreateStatsStmt() != nil:
		return []DDLStatement{createStatistics(node.GetCreateStatsStmt())}
	case node.GetUpdateStmt() != nil:
		upd := node.GetUpdateStmt()
		return []DDLStatement{{Kind: DDLUpdate, Relation: rangeVarName(upd.GetRelation()),
			Columns: targetNames(upd.GetTargetList()), HasWhere: upd.GetWhereClause() != nil}}
	case node.GetDeleteStmt() != nil:
		del := node.GetDeleteStmt()
		return []DDLStatement{{Kind: DDLDelete, Relation: rangeVarName(del.GetRelation()),
			HasWhere: del.GetWhereClause() != nil}}
	case node.GetInsertStmt() != nil:
		ins := node.GetInsertStmt()
		return []DDLStatement{{Kind: DDLInsert, Relation: rangeVarName(ins.GetRelation()),
			Columns: targetNames(ins.GetCols())}}
	case node.GetTransactionStmt() != nil:
		return []DDLStatement{{Kind: DDLTransaction,
			Transaction: transactionName(node.GetTransactionStmt())}}
	case node.GetVariableSetStmt() != nil:
		return []DDLStatement{{Kind: DDLSet, Setting: settingOf(node.GetVariableSetStmt())}}
	}
	return []DDLStatement{{Kind: DDLOther}}
}

// targetNames lists the column names of UPDATE SET targets and INSERT
// column lists.
func targetNames(targets []*pg_query.Node) []string {
	var out []string
	for _, target := range targets {
		if name := target.GetResTarget().GetName(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func transactionName(txn *pg_query.TransactionStmt) string {
	return strings.ToLower(strings.TrimPrefix(txn.GetKind().String(), "TRANS_STMT_"))
}

func createStatistics(stats *pg_query.CreateStatsStmt) DDLStatement {
	out := DDLStatement{Kind: DDLCreateStatistics, Object: nameFromList(stats.GetDefnames())}
	if rels := stats.GetRelations(); len(rels) > 0 && rels[0].GetRangeVar() != nil {
		out.Relation = rangeVarName(rels[0].GetRangeVar())
	}
	for _, expr := range stats.GetExprs() {
		if name := expr.GetStatsElem().GetName(); name != "" {
			out.Columns = append(out.Columns, name)
		}
	}
	return out
}
