package migration

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// unvalidatedLabel heads the chain link that shows model SQL pg_sage did
// not validate. That text is never a recommendation (security-05).
const unvalidatedLabel = "Model text, not validated by pg_sage"

// validatedLabel heads the chain link that shows model SQL pg_sage did
// validate; it is still model output, never a recommendation (migration-10).
const validatedLabel = "Model text, validated by pg_sage; not a recommendation"

// modelTarget is the one table a model script may touch. Schema is ""
// when the original statement relied on the search_path.
type modelTarget struct {
	Schema string
	Table  string
}

// allowedSettings are the only parameters a model script may SET/RESET.
var allowedSettings = map[string]bool{"lock_timeout": true, "statement_timeout": true}

// allowedAlterCommands are the ALTER TABLE subcommands of the migration
// patterns pg_sage recommends. Trigger, owner, RLS and storage changes
// are not among them.
var allowedAlterCommands = map[string]bool{
	"AT_AddColumn": true, "AT_ColumnDefault": true, "AT_DropNotNull": true,
	"AT_SetNotNull": true, "AT_AddConstraint": true, "AT_ValidateConstraint": true,
	"AT_DropConstraint": true, "AT_AlterColumnType": true, "AT_DropColumn": true,
	"AT_SetStatistics": true,
}

// allowedRenames are the objects a model script may rename on the target.
var allowedRenames = map[string]bool{"OBJECT_COLUMN": true, "OBJECT_TABCONSTRAINT": true}

// validateModelDDL accepts a model-authored script only when every
// statement is an ALTER TABLE, a column or constraint rename, CREATE
// [UNIQUE] INDEX [CONCURRENTLY], UPDATE, ANALYZE, SET/RESET of
// lock_timeout or statement_timeout, BEGIN or COMMIT, and every relation
// it names is the target table. A build without the parser validates
// nothing (sqlast.ErrUnavailable).
func validateModelDDL(sql string, target modelTarget) error {
	if target.Table == "" {
		return errors.New("the target table is unknown")
	}
	stmts, err := sqlast.InspectStatements(sql)
	if err != nil {
		return err
	}
	if len(stmts) == 0 {
		return errors.New("the script has no statements")
	}
	for i, st := range stmts {
		if err := checkModelStatement(st, target); err != nil {
			return fmt.Errorf("statement %d (%s): %w", i+1, st.Node, err)
		}
	}
	return checkTransactionShape(stmts)
}

// checkTransactionShape refuses a CONCURRENTLY statement after BEGIN and
// before its COMMIT: PostgreSQL rejects it there, so the script cannot run
// as written (migration-10).
func checkTransactionShape(stmts []sqlast.Statement) error {
	inBlock := false
	for i, st := range stmts {
		switch {
		case st.Kind == sqlast.KindBegin:
			inBlock = true
		case st.Kind == sqlast.KindCommit:
			inBlock = false
		case st.Concurrent && inBlock:
			return fmt.Errorf("statement %d runs CONCURRENTLY inside BEGIN/COMMIT, "+
				"which PostgreSQL refuses in a transaction block", i+1)
		}
	}
	return nil
}

func checkModelStatement(st sqlast.Statement, target modelTarget) error {
	if err := checkModelKind(st); err != nil {
		return err
	}
	if st.Kind == sqlast.KindAnalyze && len(st.Relations) == 0 {
		return errors.New("ANALYZE must name the target table")
	}
	for _, rel := range st.Relations {
		if !target.matches(rel) {
			return fmt.Errorf("names %s, which is not the target table", rel.String())
		}
	}
	// x.name runs name(x) when x has no such column, so attribute
	// references are checked as calls too.
	for _, list := range [][]sqlast.QualifiedName{st.Functions, st.AttributeCalls} {
		for _, fn := range list {
			if deniedModelFunction(fn.Name) {
				return fmt.Errorf("calls %s, an administrative function", fn.String())
			}
		}
	}
	return nil
}

func checkModelKind(st sqlast.Statement) error {
	switch st.Kind {
	case sqlast.KindAlterTable:
		if !st.TableTarget {
			return errors.New("alters something other than a table")
		}
		return checkAll(st.Commands, allowedAlterCommands, "ALTER TABLE subcommand")
	case sqlast.KindRename:
		if !st.TableTarget {
			return errors.New("renames something other than a table's parts")
		}
		return checkAll(st.Commands, allowedRenames, "rename of")
	case sqlast.KindSet, sqlast.KindReset:
		if !allowedSettings[st.Setting] {
			return fmt.Errorf("sets %q; only lock_timeout and statement_timeout may change",
				st.Setting)
		}
	case sqlast.KindUpdate:
		if st.NestedWrite {
			return errors.New("contains a data-modifying common table expression")
		}
	case sqlast.KindCreateIndex, sqlast.KindAnalyze, sqlast.KindBegin, sqlast.KindCommit:
	default:
		return errors.New("is not a statement kind model scripts may use")
	}
	return nil
}

func checkAll(items []string, allowed map[string]bool, what string) error {
	for _, item := range items {
		if !allowed[item] {
			return fmt.Errorf("%s %s is not allowed", what, item)
		}
	}
	return nil
}

// deniedModelFunction refuses server-administration functions (signal
// backends, read files, change settings, run remote or large-object I/O).
func deniedModelFunction(name string) bool {
	return strings.HasPrefix(name, "pg_") || strings.HasPrefix(name, "dblink") ||
		strings.HasPrefix(name, "lo_") || name == "set_config" ||
		strings.Contains(name, "_to_xml")
}

// matches reports whether rel is the target. Names compare as the parser
// resolved them (quoted names keep their case). An unqualified name is
// the target only when the target is unqualified or in public.
func (t modelTarget) matches(rel sqlast.QualifiedName) bool {
	if rel.Name != t.Table {
		return false
	}
	switch rel.Schema {
	case "":
		return t.Schema == "" || t.Schema == "public"
	case t.Schema:
		return true
	default:
		return t.Schema == "" && rel.Schema == "public"
	}
}

// statementTarget is the single relation the original statement names,
// or the zero target when it names none or several.
func statementTarget(sql string) modelTarget {
	stmts, err := sqlast.InspectStatements(sql)
	if err != nil || len(stmts) != 1 || len(stmts[0].Relations) != 1 {
		return modelTarget{}
	}
	rel := stmts[0].Relations[0]
	return modelTarget{Schema: rel.Schema, Table: rel.Name}
}

// unvalidatedLink shows model text pg_sage did not validate, with the
// reason, as evidence rather than as a recommendation.
func unvalidatedLink(order int, text, reason string) rca.ChainLink {
	return rca.ChainLink{
		Order:       order,
		Signal:      "model_text_unvalidated",
		Description: unvalidatedLabel + " (" + reason + ")",
		Evidence:    text,
	}
}
