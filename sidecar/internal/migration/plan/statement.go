package plan

import (
	"errors"
	"fmt"
	"strings"
)

// maxIdentBytes is NAMEDATALEN-1: PostgreSQL silently truncates longer
// names, so a derived name past it would not be the name we planned.
const maxIdentBytes = 63

// maxOptionEcho bounds how much refused SQL an error message repeats.
const maxOptionEcho = 60

var errUnsupportedStatement = errors.New("unsupported migration statement")

// statement is one ALTER TABLE clause the planner can rewrite, with
// every identifier already folded the way PostgreSQL resolves it.
type statement struct {
	classification Classification
	schema, table  string   // ALTER TABLE target; schema "" when unqualified
	constraint     string   // ADD CONSTRAINT ... UNIQUE
	columns        []string // UNIQUE column list
	timing         string   // " DEFERRABLE INITIALLY DEFERRED" or ""
	column         string   // ALTER COLUMN ... SET NOT NULL
}

// parseStatement reads the single clause validateStatement accepted.
// Anything it cannot carry into the plan verbatim is refused, never
// dropped: a plan with different semantics must not be reported as
// the requested migration.
func parseStatement(sql string) (statement, error) {
	sql = strings.TrimRight(strings.TrimSpace(sql), "; \t\r\n")
	toks, err := tokenize(sql)
	if err != nil {
		return statement{}, err
	}
	p := &parser{sql: sql, toks: toks}
	schema, table, err := p.alterTarget()
	if err != nil {
		return statement{}, err
	}
	var stmt statement
	switch {
	case p.keyword("ADD", "CONSTRAINT"):
		stmt, err = p.addUnique()
	case p.keyword("ALTER", "COLUMN"):
		stmt, err = p.setNotNull()
	default:
		err = errUnsupportedStatement
	}
	stmt.schema, stmt.table = schema, table
	return stmt, err
}

// alterTarget reads the ALTER TABLE target. PostgreSQL allows space
// around the dot, which validateStatement's pattern does not see.
func (p *parser) alterTarget() (schema, table string, err error) {
	if !p.keyword("ALTER", "TABLE") {
		return "", "", errUnsupportedStatement
	}
	p.keyword("IF", "EXISTS")
	p.keyword("ONLY")
	if table, err = p.ident("table name"); err != nil {
		return "", "", err
	}
	if p.punct(".") {
		schema = table
		if table, err = p.ident("table name"); err != nil {
			return "", "", err
		}
	}
	return schema, table, nil
}

// checkTarget refuses a statement whose parsed target is not the
// table the plan will be written against.
func checkTarget(stmt statement, facts TableFacts) error {
	wantSchema := facts.Schema
	if wantSchema == "" {
		wantSchema = "public"
	}
	if stmt.table != facts.Name || (stmt.schema != "" && stmt.schema != wantSchema) {
		return fmt.Errorf("migration SQL targets %q but the request table is %s.%s",
			strings.Trim(stmt.schema+"."+stmt.table, "."), wantSchema, facts.Name)
	}
	return nil
}

func (p *parser) addUnique() (statement, error) {
	name, err := p.ident("constraint name")
	if err != nil {
		return statement{}, err
	}
	if !p.keyword("UNIQUE") {
		return statement{}, errUnsupportedStatement
	}
	if p.peekKeyword("NULLS") {
		return statement{}, unsupportedOption(p.rest())
	}
	columns, err := p.columnList()
	if err != nil {
		return statement{}, err
	}
	timing, err := p.constraintTiming()
	if err != nil {
		return statement{}, err
	}
	return statement{classification: ClassificationAddUnique,
		constraint: name, columns: columns, timing: timing}, nil
}

func (p *parser) setNotNull() (statement, error) {
	column, err := p.ident("column name")
	if err != nil {
		return statement{}, err
	}
	if !p.keyword("SET", "NOT", "NULL") {
		return statement{}, errUnsupportedStatement
	}
	if !p.done() {
		return statement{}, unsupportedOption(p.rest())
	}
	return statement{classification: ClassificationSetNotNull, column: column}, nil
}

func (p *parser) columnList() ([]string, error) {
	if !p.punct("(") {
		return nil, fmt.Errorf("%w: UNIQUE needs a parenthesized column list",
			errUnsupportedStatement)
	}
	if p.punct(")") {
		return nil, fmt.Errorf("%w: empty column list in UNIQUE ()",
			errUnsupportedStatement)
	}
	var columns []string
	seen := map[string]bool{}
	for {
		column, err := p.ident("column name")
		if err != nil {
			return nil, err
		}
		if seen[column] {
			return nil, fmt.Errorf("%w: column %q appears twice in the UNIQUE "+
				"column list", errUnsupportedStatement, column)
		}
		seen[column] = true
		columns = append(columns, column)
		if p.punct(")") {
			return columns, nil
		}
		if !p.punct(",") {
			return nil, fmt.Errorf("%w: UNIQUE column list must name plain "+
				"columns; plan it by hand", errUnsupportedStatement)
		}
	}
}

// constraintTiming accepts only [NOT] DEFERRABLE and INITIALLY
// DEFERRED|IMMEDIATE, which ADD CONSTRAINT ... USING INDEX also takes,
// and renders them in canonical order.
func (p *parser) constraintTiming() (string, error) {
	clauses := [][]string{
		{"DEFERRABLE"}, {"NOT", "DEFERRABLE"},
		{"INITIALLY", "DEFERRED"}, {"INITIALLY", "IMMEDIATE"},
	}
	var deferrable, initially string
	for !p.done() {
		start := p.pos
		matched := ""
		for _, clause := range clauses {
			if p.keyword(clause...) {
				matched = strings.Join(clause, " ")
				break
			}
		}
		slot := &deferrable
		if strings.HasPrefix(matched, "INITIALLY") {
			slot = &initially
		}
		switch {
		case matched == "":
			return "", unsupportedOption(p.restFrom(start))
		case *slot != "":
			return "", fmt.Errorf("conflicting constraint timing %q after %q; "+
				"plan it by hand", matched, *slot)
		}
		*slot = matched
	}
	if deferrable == "NOT DEFERRABLE" && initially == "INITIALLY DEFERRED" {
		return "", fmt.Errorf("a constraint declared INITIALLY DEFERRED must be " +
			"DEFERRABLE; plan it by hand")
	}
	timing := ""
	for _, clause := range []string{deferrable, initially} {
		if clause != "" {
			timing += " " + clause
		}
	}
	return timing, nil
}

func unsupportedOption(text string) error {
	if len(text) > maxOptionEcho {
		cut := maxOptionEcho
		for cut > 0 && text[cut]&0xC0 == 0x80 {
			cut--
		}
		text = text[:cut] + "..."
	}
	return fmt.Errorf("unsupported option %q; plan it by hand", text)
}

// checkNames refuses any emitted identifier PostgreSQL would truncate.
func checkNames(names ...string) error {
	for _, name := range names {
		if len(name) > maxIdentBytes {
			return fmt.Errorf("name %q is %d bytes; PostgreSQL truncates names "+
				"longer than %d bytes; plan it by hand", name, len(name), maxIdentBytes)
		}
	}
	return nil
}
