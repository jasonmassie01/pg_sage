package analyzer

import (
	"context"

	"github.com/pg-sage/sidecar/internal/collector"
)

// FactFilter applies the database's operator-confirmed facts to a cycle
// (roadmap 2.3): ExcludeSnapshot removes test-fixture schemas before any
// rule runs (and returns them); ApplyFindings redirects the findings that
// facts bind and adds the cleanup batches, returning the categories it
// evaluated.
type FactFilter interface {
	ExcludeSnapshot(ctx context.Context, snaps ...*collector.Snapshot) []string
	ApplyFindings(ctx context.Context, findings []Finding, excluded []string) ([]Finding,
		[]string)
}

// WithFactFilter installs the confirmed-facts filter. Call before Run.
func (a *Analyzer) WithFactFilter(f FactFilter) {
	a.factFilter = f
}

// excludeFactSchemas removes confirmed test fixtures from the cycle's
// snapshots; it returns the excluded schemas of current.
func (a *Analyzer) excludeFactSchemas(ctx context.Context,
	current, previous *collector.Snapshot) []string {
	if a.factFilter == nil {
		return nil
	}
	return a.factFilter.ExcludeSnapshot(ctx, current, previous)
}

// applyFactFilter applies the confirmed facts to the cycle's findings.
func (a *Analyzer) applyFactFilter(ctx context.Context, findings []Finding,
	excluded []string) []Finding {
	if a.factFilter == nil {
		return findings
	}
	out, evaluated := a.factFilter.ApplyFindings(ctx, findings, excluded)
	a.eval.evaluated(evaluated...)
	return out
}
