//go:build cgo

package sqlast

import (
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// readUnique fills alter from an ADD CONSTRAINT the tree parsed. The
// tree is authoritative for the constraint; the scanner tokens only
// locate the written text a refusal repeats and the timing clauses as
// written, which the tree folds into two booleans.
func readUnique(sql string, tokens []*pg_query.ScanToken, con *pg_query.Constraint,
	alter *PlannableAlter) error {
	if con.GetContype() != pg_query.ConstrType_CONSTR_UNIQUE {
		return fmt.Errorf("%w: ADD CONSTRAINT is planned for UNIQUE only, not %s; "+
			"plan it by hand", ErrNotPlannable,
			strings.TrimPrefix(con.GetContype().String(), "CONSTR_"))
	}
	if con.GetConname() == "" {
		return fmt.Errorf("%w: ADD UNIQUE needs a constraint name to name its index; "+
			"write ADD CONSTRAINT name UNIQUE (...)", ErrNotPlannable)
	}
	trailing, err := afterKeyList(sql, tokens)
	if err != nil {
		return err
	}
	if field := uncarriedField(con); field != "" {
		where := field
		if len(trailing) > 0 {
			where = textFrom(sql, trailing[0])
		}
		return unsupportedOption(where)
	}
	columns, err := keyColumns(con.GetKeys())
	if err != nil {
		return err
	}
	timing, err := readTiming(sql, trailing, con)
	if err != nil {
		return err
	}
	alter.Kind, alter.Constraint, alter.Columns = AlterAddUnique, con.GetConname(), columns
	alter.Deferrable, alter.InitDeferred, alter.Timing =
		con.GetDeferrable(), con.GetInitdeferred(), timing
	return nil
}

// afterKeyList returns the tokens after the UNIQUE column list. UNIQUE is
// a reserved word, so its first bare occurrence is the constraint's; the
// list must follow it at once (NULLS [NOT] DISTINCT and USING INDEX are
// refused), and a column name cannot contain a ')' token.
func afterKeyList(sql string, tokens []*pg_query.ScanToken) ([]*pg_query.ScanToken, error) {
	for i, tok := range tokens {
		if tok.GetToken() != pg_query.Token_UNIQUE {
			continue
		}
		if i+1 >= len(tokens) || tokens[i+1].GetToken() != pg_query.Token_ASCII_40 {
			if i+1 >= len(tokens) {
				return nil, unsupportedOption("")
			}
			return nil, unsupportedOption(textFrom(sql, tokens[i+1]))
		}
		for j := i + 2; j < len(tokens); j++ {
			if tokens[j].GetToken() == pg_query.Token_ASCII_41 {
				return tokens[j+1:], nil
			}
		}
		break
	}
	return nil, fmt.Errorf("%w: cannot find the UNIQUE column list; plan it by hand",
		ErrNotPlannable)
}

// uncarriedField names a constraint property the rewritten plan would
// drop: CREATE UNIQUE INDEX ... plus USING INDEX attaches none of them.
func uncarriedField(con *pg_query.Constraint) string {
	switch {
	case len(con.GetIncluding()) > 0:
		return "INCLUDE"
	case len(con.GetOptions()) > 0:
		return "WITH"
	case con.GetIndexspace() != "":
		return "USING INDEX TABLESPACE"
	case con.GetIndexname() != "":
		return "USING INDEX"
	case con.GetNullsNotDistinct():
		return "NULLS NOT DISTINCT"
	case con.GetSkipValidation():
		return "NOT VALID"
	case con.GetIsNoInherit():
		return "NO INHERIT"
	case con.GetAccessMethod() != "" || con.GetWhereClause() != nil ||
		len(con.GetExclusions()) > 0:
		return "index method or predicate"
	}
	return ""
}

func keyColumns(keys []*pg_query.Node) ([]string, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: UNIQUE needs a column list; plan it by hand",
			ErrNotPlannable)
	}
	columns := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		column := key.GetString_().GetSval()
		if column == "" {
			return nil, fmt.Errorf("%w: UNIQUE column list must name plain columns; "+
				"plan it by hand", ErrNotPlannable)
		}
		if seen[column] {
			return nil, fmt.Errorf("%w: column %q appears twice in the UNIQUE "+
				"column list", ErrNotPlannable, column)
		}
		seen[column] = true
		columns = append(columns, column)
	}
	return columns, nil
}

func unsupportedOption(text string) error {
	return fmt.Errorf("%w %q; plan it by hand", ErrUnsupportedOption, echo(text))
}
