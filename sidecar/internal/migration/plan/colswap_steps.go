package plan

import (
	"fmt"
	"strings"
)

const lockAccessExclusive = "ACCESS EXCLUSIVE"

// buildSwapSteps lays out the script in phase order.
func buildSwapSteps(f SwapFacts, req SwapRequest, n swapNames) []SwapStep {
	steps := prepareSteps(f, req, n)
	steps = append(steps, backfillStep(f, req, n))
	steps = append(steps, verifySteps(f, n)...)
	steps = append(steps, swapTransaction(f, req, n)...)
	steps = append(steps, finishSteps(f, n)...)
	return append(steps, SwapStep{Name: "drop_old_column", Phase: SwapPhaseContract,
		LockLevel: lockAccessExclusive, Destructive: true,
		SQL: fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", n.table, n.oldCol),
		Note: "Run once the application no longer reads the old column; it holds the " +
			"pre-swap values for rollback until then."})
}

func prepareSteps(f SwapFacts, req SwapRequest, n swapNames) []SwapStep {
	trigger := SwapStep{Name: "create_sync_trigger", Phase: SwapPhasePrepare,
		LockLevel: "SHARE ROW EXCLUSIVE",
		SQL: fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW "+
			"EXECUTE FUNCTION %s()", n.trigger, n.table, n.function)}
	if !castCannotFail(f.Column.Type, req.TargetType) {
		trigger.Note = fmt.Sprintf("From this step on, a write whose %s value does not "+
			"cast to %s fails.", f.Column.Name, req.TargetType)
	}
	steps := []SwapStep{
		{Name: "set_lock_timeout", Phase: SwapPhasePrepare, LockLevel: LockNone,
			SQL:  fmt.Sprintf("SET lock_timeout = '%dms'", req.LockTimeout.Milliseconds()),
			Note: "Run every step in this one session."},
		{Name: "set_plan_cache_mode", Phase: SwapPhasePrepare, LockLevel: LockNone,
			SQL:  "SET plan_cache_mode = force_custom_plan",
			Note: "A generic plan of the backfill cannot use the key's index for $1."},
		{Name: "add_column", Phase: SwapPhasePrepare, LockLevel: lockAccessExclusive,
			SQL: fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", n.table, n.newCol,
				req.TargetType)},
		{Name: "create_sync_function", Phase: SwapPhasePrepare, LockLevel: LockNone,
			SQL: syncFunctionSQL(n, req.TargetType)},
		trigger,
	}
	if f.Column.NotNull {
		steps = append(steps, SwapStep{Name: "add_not_null_proof", Phase: SwapPhasePrepare,
			LockLevel: lockAccessExclusive,
			SQL: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) "+
				"NOT VALID", n.table, n.proof, n.newCol)})
	}
	return steps
}

// castCannotFail: widening within the integer family never fails.
func castCannotFail(from, to string) bool {
	have, ok := integerRank[from]
	return ok && integerRank[to] >= have
}

func syncFunctionSQL(n swapNames, target string) string {
	return fmt.Sprintf("CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS %s\n"+
		"BEGIN\n  NEW.%s := NEW.%s::%s;\n  RETURN NEW;\nEND\n%s",
		n.function, swapDollarTag, n.newCol, n.column, target, swapDollarTag)
}

// backfillStep copies one batch, walking the primary key: $1 is the last
// key of the previous batch (NULL first) and the batch returns its keys.
func backfillStep(f SwapFacts, req SwapRequest, n swapNames) SwapStep {
	keyType := f.PrimaryKey.ColumnType
	return SwapStep{Name: "backfill", Phase: SwapPhaseBackfill, LockLevel: "ROW EXCLUSIVE",
		Repeat: true, DML: true,
		SQL: fmt.Sprintf("WITH batch AS (SELECT %[1]s FROM %[2]s WHERE $1::%[3]s IS NULL "+
			"OR %[1]s > $1::%[3]s ORDER BY %[1]s LIMIT %[4]d) UPDATE %[2]s AS t SET %[5]s = "+
			"t.%[6]s::%[7]s FROM batch WHERE t.%[1]s = batch.%[1]s RETURNING t.%[1]s",
			n.key, n.table, keyType, req.BatchSize, n.newCol, n.column, req.TargetType),
		Note: "Repeat with $1 = the largest key the last batch returned (NULL for the " +
			"first) until a batch returns no rows; each batch is its own transaction."}
}

func verifySteps(f SwapFacts, n swapNames) []SwapStep {
	var steps []SwapStep
	if f.Column.NotNull {
		steps = append(steps, SwapStep{Name: "validate_not_null_proof", Phase: SwapPhaseVerify,
			LockLevel: "SHARE UPDATE EXCLUSIVE",
			SQL:       fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", n.table, n.proof)})
	}
	if swapsPrimaryKey(f) {
		steps = append(steps, SwapStep{Name: "build_unique_index", Phase: SwapPhaseVerify,
			LockLevel: "SHARE UPDATE EXCLUSIVE", RequiresTopLevel: true,
			SQL: fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY %s ON %s (%s)",
				n.index, n.table, n.newCol),
			Note: fmt.Sprintf("If it fails it leaves an invalid index: DROP INDEX "+
				"CONCURRENTLY %s.%s, then run it again.", quote(f.Schema), n.index)})
	}
	return steps
}

func swapsPrimaryKey(f SwapFacts) bool {
	return f.PrimaryKey != nil && f.PrimaryKey.Columns[0] == f.Column.Name
}

func finishSteps(f SwapFacts, n swapNames) []SwapStep {
	var steps []SwapStep
	for _, fk := range f.Referencing {
		steps = append(steps, SwapStep{Name: "validate_referencing_fk", Phase: SwapPhaseFinish,
			LockLevel: "SHARE UPDATE EXCLUSIVE",
			SQL: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", refTable(fk),
				quote(fk.Constraint))})
	}
	return append(steps, SwapStep{Name: "drop_sync_function", Phase: SwapPhaseFinish,
		LockLevel: lockAccessExclusive, SQL: fmt.Sprintf("DROP FUNCTION %s()", n.function),
		Note: "The lock is on the function, not on a table."})
}

// lockTablesSQL locks the referencing tables before the target: a write
// to a referencing table also locks the target for its key check, so
// this order does not wait on a writer that waits on the swap.
func lockTablesSQL(f SwapFacts, n swapNames) string {
	var tables []string
	seen := map[string]bool{n.table: true}
	for _, fk := range f.Referencing {
		if t := refTable(fk); !seen[t] {
			seen[t] = true
			tables = append(tables, t)
		}
	}
	tables = append(tables, n.table)
	return "LOCK TABLE " + strings.Join(tables, ", ") + " IN ACCESS EXCLUSIVE MODE"
}
