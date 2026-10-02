// Package slobudget adapts a database's Sage SRE M5 SLO engine to the
// earned-autonomy error-budget signal. It is separate so the ledger (which
// the executor imports) does not import the SLO engine.
package slobudget

import (
	"context"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// Source reads one engine's budget summary in the ledger's shape.
type Source struct{ engine slo.ErrorBudgetSource }

var _ earned.BudgetSummarySource = Source{}

// New is the budget signal of a database's SLO engine; nil (no SLO
// subsystem, so no budget that could burn) when the engine is nil.
func New(e *slo.Engine) earned.BudgetSource {
	if e == nil {
		return nil
	}
	return earned.NewSummaryBudget(Source{engine: e})
}

// BudgetSummary copies the engine's summary field by field.
func (s Source) BudgetSummary(ctx context.Context) (earned.BudgetSummary, error) {
	sum, err := s.engine.BudgetSummary(ctx)
	if err != nil {
		return earned.BudgetSummary{}, err
	}
	return earned.BudgetSummary{Database: sum.Database, FastBurning: sum.FastBurning,
		AppFastBurning: sum.AppFastBurning, AppSLOs: sum.AppSLOs,
		Unknown: sum.Unknown, UnknownApp: sum.UnknownApp}, nil
}
