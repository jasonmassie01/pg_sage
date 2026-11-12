package plan

import (
	"fmt"
	"strings"
)

// swapDollarTag quotes the sync function's body.
const swapDollarTag = "$swap$"

// swapNames are the quoted names one swap uses: the target table, the
// columns, and the objects it creates for the duration of the swap.
type swapNames struct {
	raw                []string // every emitted identifier, unquoted
	table, key, keyCol string   // key is the primary key column
	column, newCol     string
	oldCol             string
	function, trigger  string // trigger is the bare trigger name
	proof, index       string // bare constraint and index names
	pkey               string
	sequence           string
}

func newSwapNames(f SwapFacts, req SwapRequest) swapNames {
	base := f.Table + "_" + f.Column.Name + "_swap"
	col := f.Column.Name
	n := swapNames{
		raw: []string{f.Schema, f.Table, col, col + "__swap_new", col + "__swap_old",
			base + "_sync", base + "_nn", base + "_key"},
		table:  qualified(TableFacts{Schema: f.Schema, Name: f.Table}),
		column: quote(col), newCol: quote(col + "__swap_new"),
		oldCol:   quote(col + "__swap_old"),
		function: quote(f.Schema) + "." + quote(base+"_sync"),
		trigger:  quote(base + "_sync"), proof: quote(base + "_nn"),
		index: quote(base + "_key"),
	}
	if f.PrimaryKey != nil && len(f.PrimaryKey.Columns) == 1 {
		n.keyCol = f.PrimaryKey.Columns[0]
		n.key = quote(n.keyCol)
		n.pkey = quote(f.PrimaryKey.Constraint)
		n.raw = append(n.raw, n.keyCol, f.PrimaryKey.Constraint)
	}
	if f.Column.SequenceName != "" {
		n.sequence = quote(f.Column.SequenceSchema) + "." + quote(f.Column.SequenceName)
		n.raw = append(n.raw, f.Column.SequenceSchema, f.Column.SequenceName)
	}
	for _, fk := range f.Referencing {
		n.raw = append(n.raw, fk.Constraint, fk.Schema, fk.Table, fk.Column)
	}
	return n
}

// check refuses a name PostgreSQL would truncate, and one holding the
// sync function's dollar tag, which could end its body early.
func (n swapNames) check() error {
	if err := checkNames(n.raw...); err != nil {
		return err
	}
	for _, name := range n.raw {
		if strings.Contains(name, swapDollarTag) {
			return fmt.Errorf("name %q holds %s, the sync function's quoting tag; "+
				"plan it by hand", name, swapDollarTag)
		}
	}
	return nil
}

// refTable is a referencing foreign key's quoted table.
func refTable(fk SwapForeignKey) string {
	return qualified(TableFacts{Schema: fk.Schema, Name: fk.Table})
}
