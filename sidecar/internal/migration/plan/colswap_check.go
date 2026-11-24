package plan

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// swapTargetTypes are the types a swap can produce, spelled as
// format_type prints them, so the plan's SQL and the facts agree.
var swapTargetTypes = map[string]bool{
	"smallint": true, "integer": true, "bigint": true, "numeric": true,
	"text": true, "uuid": true, "date": true, "boolean": true, "jsonb": true,
	"timestamp with time zone": true, "timestamp without time zone": true,
}

// integerRank orders the integer family; a key or sequence may only move
// within it, where the comparison operators cross types.
var integerRank = map[string]int{"smallint": 1, "integer": 2, "bigint": 3}

func swapTargetList() string {
	names := make([]string, 0, len(swapTargetTypes))
	for name := range swapTargetTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// reSeqDefault is the default serial and bigserial columns get.
var reSeqDefault = regexp.MustCompile(`^nextval\('[^']+'::regclass\)$`)

// checkSwapFacts refuses every shape the swap does not carry over
// exactly. A refusal names the reason; nothing is planned in part.
func checkSwapFacts(f SwapFacts, req SwapRequest) error {
	if f.Schema != req.Schema || f.Table != req.Table || f.Column.Name != req.Column {
		return fmt.Errorf("the request targets %s.%s.%s but the catalog facts describe %s.%s.%s",
			req.Schema, req.Table, req.Column, f.Schema, f.Table, f.Column.Name)
	}
	if err := checkSwapTable(f); err != nil {
		return err
	}
	if err := checkSwapColumn(f.Column, req.TargetType); err != nil {
		return err
	}
	if len(f.Dependents) > 0 {
		return fmt.Errorf("%s depends on the column and would be lost with the old "+
			"column; plan it by hand", strings.Join(f.Dependents, ", "))
	}
	if len(f.Outgoing) > 0 {
		return fmt.Errorf("foreign key on the column (%s) is not carried over; plan it "+
			"by hand", strings.Join(f.Outgoing, ", "))
	}
	return checkSwapKeys(f, req.TargetType)
}

func checkSwapTable(f SwapFacts) error {
	switch {
	case f.RelKind == "p":
		return errors.New("the table is partitioned; out of scope")
	case f.RelKind != "r":
		return fmt.Errorf("relation kind %q is not an ordinary table (a view or "+
			"other relation); out of scope", f.RelKind)
	case f.IsPartition:
		return errors.New("the table is a partition; out of scope")
	case f.HasInheritance:
		return errors.New("the table takes part in inheritance; out of scope")
	case f.PrimaryKey == nil:
		return errors.New("the table has no primary key to batch the backfill by")
	case len(f.PrimaryKey.Columns) != 1:
		return fmt.Errorf("the backfill needs a single-column primary key; %s has %d "+
			"columns", f.PrimaryKey.Constraint, len(f.PrimaryKey.Columns))
	}
	return nil
}

func checkSwapColumn(c SwapColumn, target string) error {
	switch {
	case c.Type == target:
		return fmt.Errorf("the column is already %s", target)
	case c.Identity:
		return errors.New("an identity column's sequence cannot be moved by the swap; " +
			"plan it by hand")
	case c.Generated:
		return errors.New("a generated column is computed, not copied; plan it by hand")
	case c.Default != "" && (!c.DefaultIsSeq || !reSeqDefault.MatchString(c.Default)):
		return fmt.Errorf("column default %s is not the column's own sequence; plan it "+
			"by hand", c.Default)
	}
	if c.SequenceName == "" {
		return nil
	}
	have, ok := integerRank[c.SequenceType]
	want, wantOK := integerRank[target]
	if !ok || !wantOK || have > want {
		return fmt.Errorf("the column's sequence is %s, which a %s column cannot hold; "+
			"plan it by hand", c.SequenceType, target)
	}
	return nil
}

// checkSwapKeys: a primary key the swap moves must be one it rebuilds
// exactly; foreign keys must be validated, reference the column through
// a single-column key, from an ordinary table, with a referencing column
// that stays comparable with the new type.
func checkSwapKeys(f SwapFacts, target string) error {
	if err := checkSwapPrimaryKey(f); err != nil {
		return err
	}
	for _, fk := range f.Referencing {
		switch {
		case fk.ChildPartitioned || fk.ChildIsPartition:
			return fmt.Errorf("foreign key %s comes from a partitioned table; out of "+
				"scope", fk.Constraint)
		case fk.NotValidated:
			return fmt.Errorf("foreign key %s is not validated (NOT VALID or NOT ENFORCED); "+
				"re-adding it would enforce it, so plan it by hand", fk.Constraint)
		case fk.Columns != 1:
			return fmt.Errorf("multi-column foreign key %s; plan it by hand", fk.Constraint)
		case fk.SetColumns:
			return fmt.Errorf("foreign key %s has an ON DELETE SET NULL column list; "+
				"plan it by hand", fk.Constraint)
		case integerRank[fk.ColumnType] == 0 || integerRank[target] == 0:
			return fmt.Errorf("referencing column %s.%s.%s is %s and cannot reference a "+
				"%s key; change the referencing columns first", fk.Schema, fk.Table,
				fk.Column, fk.ColumnType, target)
		}
	}
	return nil
}

// checkSwapPrimaryKey refuses a key the swap would re-attach changed: it
// rebuilds a plain, immediate unique index on the new column only.
func checkSwapPrimaryKey(f SwapFacts) error {
	if !swapsPrimaryKey(f) {
		return nil
	}
	k := f.PrimaryKey
	switch {
	case k.Deferrable:
		return fmt.Errorf("primary key %s is deferrable; plan it by hand", k.Constraint)
	case k.Include:
		return fmt.Errorf("primary key %s has INCLUDE columns; plan it by hand", k.Constraint)
	case k.ReplicaIdentity:
		return fmt.Errorf("primary key %s is the table's replica identity index; plan it "+
			"by hand", k.Constraint)
	}
	return nil
}
