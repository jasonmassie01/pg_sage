package earned

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// BudgetSummary mirrors Sage SRE M5's slo.BudgetSummary (same field
// names), so the integration adapter is a field copy. FastBurning covers
// every SLO, database proxies included; UnknownApp names only registered
// app SLOs whose state is unknown; AppSLOs counts the registered app SLOs.
type BudgetSummary struct {
	Database       string
	FastBurning    bool
	AppFastBurning bool
	AppSLOs        int
	Unknown        []string
	UnknownApp     []string
}

// BudgetSummarySource reads one database's error-budget summary (M5's
// slo.ErrorBudgetSource, bound to a database).
type BudgetSummarySource interface {
	BudgetSummary(ctx context.Context) (BudgetSummary, error)
}

// BudgetStateOf maps a summary to the downgrade signal. Proxy SLOs are on
// by default and are often unknown for structural reasons (no standbys,
// no log access, a baseline still building), so only an unknown app SLO
// counts as unknown. A page-level burn of any SLO, proxy included, is
// database-side evidence of harm and counts. Without app SLOs and without
// a burn there is no budget that could burn.
func BudgetStateOf(sum BudgetSummary) BudgetState {
	st := BudgetState{Configured: sum.AppSLOs > 0 || sum.FastBurning,
		FastBurning: sum.FastBurning, Unknown: len(sum.UnknownApp) > 0}
	switch {
	case st.FastBurning && sum.AppFastBurning:
		st.Detail = "an app SLO is burning its error budget at the page rate"
	case st.FastBurning:
		st.Detail = "a database proxy SLO is burning its error budget at the page rate"
	case st.Unknown:
		st.Detail = "app SLO state unknown: " + strings.Join(sum.UnknownApp, ", ")
	}
	return st
}

// SummaryBudget is a BudgetSource over a BudgetSummarySource.
type SummaryBudget struct{ source BudgetSummarySource }

// NewSummaryBudget adapts src.
func NewSummaryBudget(src BudgetSummarySource) *SummaryBudget {
	return &SummaryBudget{source: src}
}

var _ BudgetSource = (*SummaryBudget)(nil)

// ErrorBudget reads the summary and refuses one for another database.
func (b *SummaryBudget) ErrorBudget(ctx context.Context, database string) (BudgetState,
	error) {
	if b == nil || b.source == nil {
		return BudgetState{}, errors.New("error budget: no SLO summary source")
	}
	sum, err := b.source.BudgetSummary(ctx)
	if err != nil {
		return BudgetState{}, fmt.Errorf("error budget of %q: %w", database, err)
	}
	if sum.Database != "" && database != "" && sum.Database != database {
		return BudgetState{}, fmt.Errorf("error budget: summary is for %q, not %q",
			sum.Database, database)
	}
	return BudgetStateOf(sum), nil
}
