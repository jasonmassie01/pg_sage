package plan

import (
	"fmt"
	"strings"
)

// swapTransaction is the one short transaction that moves the name, the
// keys, the default and the sequence to the new column. Every statement
// in it is metadata-only: the NOT NULL is proven by the validated CHECK
// and the primary key attaches the index built concurrently.
func swapTransaction(f SwapFacts, req SwapRequest, n swapNames) []SwapStep {
	tx := []SwapStep{
		{Name: "begin", SQL: "BEGIN", LockLevel: LockNone},
		{Name: "lock_tables", SQL: lockTablesSQL(f, n), LockLevel: lockAccessExclusive,
			Note: "If the lock wait hits lock_timeout, ROLLBACK and run the transaction " +
				"again from BEGIN."},
	}
	for _, fk := range f.Referencing {
		tx = append(tx, swapStep("drop_referencing_fk", fmt.Sprintf(
			"ALTER TABLE %s DROP CONSTRAINT %s", refTable(fk), quote(fk.Constraint))))
	}
	if swapsPrimaryKey(f) {
		tx = append(tx, swapStep("drop_primary_key", fmt.Sprintf(
			"ALTER TABLE %s DROP CONSTRAINT %s", n.table, n.pkey)))
	}
	tx = append(tx, swapStep("drop_sync_trigger",
		fmt.Sprintf("DROP TRIGGER %s ON %s", n.trigger, n.table)))
	tx = append(tx, renameSteps(f, n)...)
	tx = append(tx, newColumnSteps(f, req, n)...)
	for _, fk := range f.Referencing {
		s := swapStep("add_referencing_fk", foreignKeySQL(fk, n))
		s.LockLevel = "SHARE ROW EXCLUSIVE"
		tx = append(tx, s)
	}
	tx = append(tx, SwapStep{Name: "commit", SQL: "COMMIT", LockLevel: LockNone})
	for i := range tx {
		tx[i].Phase, tx[i].InTransaction = SwapPhaseSwap, true
	}
	return tx
}

func swapStep(name, sql string) SwapStep {
	return SwapStep{Name: name, SQL: sql, LockLevel: lockAccessExclusive}
}

// renameSteps frees the old column (no default, nullable, so writes that
// no longer set it succeed) and swaps the names.
func renameSteps(f SwapFacts, n swapNames) []SwapStep {
	var steps []SwapStep
	if f.Column.Default != "" {
		steps = append(steps, swapStep("drop_old_default", fmt.Sprintf(
			"ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT", n.table, n.column)))
	}
	if f.Column.NotNull {
		steps = append(steps, swapStep("drop_old_not_null", fmt.Sprintf(
			"ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL", n.table, n.column)))
	}
	return append(steps,
		swapStep("rename_old_column", fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
			n.table, n.column, n.oldCol)),
		swapStep("rename_new_column", fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
			n.table, n.newCol, n.column)))
}

// newColumnSteps gives the renamed column the old one's NOT NULL, primary
// key, default and sequence.
func newColumnSteps(f SwapFacts, req SwapRequest, n swapNames) []SwapStep {
	var steps []SwapStep
	if f.Column.NotNull {
		steps = append(steps,
			swapStep("set_not_null", fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL",
				n.table, n.column)),
			swapStep("drop_not_null_proof", fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s",
				n.table, n.proof)))
	}
	if swapsPrimaryKey(f) {
		steps = append(steps, swapStep("attach_primary_key", fmt.Sprintf(
			"ALTER TABLE %s ADD CONSTRAINT %s PRIMARY KEY USING INDEX %s",
			n.table, n.pkey, n.index)))
	}
	if n.sequence == "" {
		return steps
	}
	if f.Column.Default != "" {
		steps = append(steps, swapStep("set_default", fmt.Sprintf(
			"ALTER TABLE %s ALTER COLUMN %s SET DEFAULT nextval('%s'::regclass)",
			n.table, n.column, strings.ReplaceAll(n.sequence, "'", "''"))))
	}
	as := ""
	if f.Column.SequenceType != req.TargetType {
		as = " AS " + req.TargetType
	}
	s := swapStep("move_sequence", fmt.Sprintf("ALTER SEQUENCE %s%s OWNED BY %s.%s",
		n.sequence, as, n.table, n.column))
	s.LockLevel = "ACCESS SHARE"
	return append(steps, s)
}

// fkActions are pg_constraint's action codes; NO ACTION is the default
// and is not spelled out.
var fkActions = map[string]string{"r": "RESTRICT", "c": "CASCADE", "n": "SET NULL",
	"d": "SET DEFAULT"}

// foreignKeySQL re-adds a referencing key, NOT VALID, with its actions,
// match type and timing, referencing the swapped column by its name.
func foreignKeySQL(fk SwapForeignKey, n swapNames) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
		refTable(fk), quote(fk.Constraint), quote(fk.Column), n.table, n.column)
	if fk.Match == "f" {
		b.WriteString(" MATCH FULL")
	}
	if a := fkActions[fk.OnUpdate]; a != "" {
		b.WriteString(" ON UPDATE " + a)
	}
	if a := fkActions[fk.OnDelete]; a != "" {
		b.WriteString(" ON DELETE " + a)
	}
	if fk.Deferrable {
		b.WriteString(" DEFERRABLE")
		if fk.Deferred {
			b.WriteString(" INITIALLY DEFERRED")
		}
	}
	b.WriteString(" NOT VALID")
	return b.String()
}
