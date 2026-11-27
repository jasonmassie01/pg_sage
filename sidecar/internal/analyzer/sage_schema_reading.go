package analyzer

import (
	"context"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// cycleSchema is one cycle's reading of the sage schema (or its failure).
type cycleSchema struct {
	cycle  *cycleEval
	schema selfcost.Schema
	err    error
}

// sageSchemaReading is the cycle's one reading of the sage schema, through
// the bounded catalog reader: the footprint check, the self-cost meter and
// the storage budget share it instead of each scanning pg_class. A cycle
// is the evaluation state (a new one each cycle); without one every call
// reads afresh.
func (a *Analyzer) sageSchemaReading(ctx context.Context) (selfcost.Schema, error) {
	if m := a.schemaMemo; m != nil && a.eval != nil && m.cycle == a.eval {
		return m.schema, m.err
	}
	sc, err := a.readSageSchema(ctx, a.catalog())
	if a.eval != nil {
		a.schemaMemo = &cycleSchema{cycle: a.eval, schema: sc, err: err}
	}
	return sc, err
}
