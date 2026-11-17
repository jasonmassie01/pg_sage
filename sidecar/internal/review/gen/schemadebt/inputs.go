package schemadebt

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/migration/plan"
)

// MaxColumns bounds the columns one review reads catalog facts for; the
// rest are reported as not read (Inputs.Truncated).
const MaxColumns = 64

// ColumnRef is a column by its catalog names.
type ColumnRef struct{ Schema, Table, Column string }

// String is schema.table.column.
func (c ColumnRef) String() string { return c.Schema + "." + c.Table + "." + c.Column }

func (c ColumnRef) table() string { return c.Schema + "." + c.Table }

// Point is one runway sample: when it was taken and its value.
type Point struct {
	At    time.Time
	Value float64
}

// Series is one sequence's samples of one epoch as the runway store
// records them in sage.runway_samples (kind sequence): a descending
// sequence is sampled negated, so every series rises toward Limit, the
// newest sample's limit_value.
type Series struct {
	Subject string
	Epoch   string
	Limit   float64
	Points  []Point
}

// Column is what the catalog says about one debt column. ReadErr is why
// its facts could not be read (Facts is then empty); Sequence is the
// runway subject of the sequence that feeds it ("" for none: no owned-by
// or identity sequence), SequenceSchema and SequenceName its catalog
// names and Increment its increment.
type Column struct {
	Facts          plan.SwapFacts
	ReadErr        string
	Sequence       string
	SequenceSchema string
	SequenceName   string
	Increment      int64
}

// Inputs is what the generator joins with the pack. A nil Columns map
// means the catalog was not read; Series are by subject. Truncated says
// some asked-for columns were left unread (MaxColumns).
type Inputs struct {
	Columns   map[ColumnRef]Column
	Series    map[string]Series
	Truncated bool
}

// Source reads the inputs for the debt columns: their catalog facts and
// their sequences' runway samples taken since since. Read must be safe
// for concurrent use: one generator serves many databases.
type Source interface {
	Read(ctx context.Context, cols []ColumnRef, since time.Time) (Inputs, error)
}
