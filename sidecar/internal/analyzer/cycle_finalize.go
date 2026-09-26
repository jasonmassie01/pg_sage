package analyzer

import (
	"context"
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/notify"
)

// dispatchCriticalFindings sends notifications for critical-severity
// findings. Only fires when a dispatcher is configured.
func (a *Analyzer) dispatchCriticalFindings(
	ctx context.Context, findings []Finding,
) {
	if a.dispatcher == nil {
		return
	}
	for _, f := range findings {
		if f.Severity != "critical" {
			continue
		}
		detail, _ := json.Marshal(f.Detail)
		event := notify.FindingCriticalEvent(
			f.Title, string(detail), a.databaseName)
		if err := a.dispatcher.Dispatch(ctx, event); err != nil {
			a.logFn("ERROR",
				"critical finding dispatch: %v", err)
		}
	}
}

// dispatchRewriteFindings sends notifications for query_tuning
// findings that include a suggested rewrite.
func (a *Analyzer) dispatchRewriteFindings(
	ctx context.Context, findings []Finding,
) {
	if a.dispatcher == nil {
		return
	}
	for _, f := range findings {
		if f.Category != "query_tuning" {
			continue
		}
		rewrite, _ := f.Detail["suggested_rewrite"].(string)
		if rewrite == "" {
			continue
		}
		query, _ := f.Detail["query"].(string)
		rationale, _ := f.Detail["rewrite_rationale"].(string)
		event := notify.QueryRewriteEvent(
			f.Title, query, rewrite, rationale,
			a.databaseName,
		)
		if err := a.dispatcher.Dispatch(ctx, event); err != nil {
			a.logFn("ERROR",
				"rewrite finding dispatch: %v", err)
		}
	}
}

// finalizeCycle publishes, persists, notifies and resolves one cycle's
// deduplicated findings. evaluated names the categories whose producers
// completed successfully this cycle.
func (a *Analyzer) finalizeCycle(
	ctx context.Context, findings []Finding, evaluated map[string]bool,
) {
	_ = evaluated
	a.mu.Lock()
	a.findings = findings
	a.mu.Unlock()

	if err := UpsertFindings(ctx, a.pool, findings); err != nil {
		a.logFn("ERROR", "analyzer: upsert findings: %v", err)
	}
	a.dispatchCriticalFindings(ctx, findings)
	a.dispatchRewriteFindings(ctx, findings)

	activeByCategory := make(map[string]map[string]bool)
	for _, f := range findings {
		if activeByCategory[f.Category] == nil {
			activeByCategory[f.Category] = make(map[string]bool)
		}
		activeByCategory[f.Category][f.ObjectIdentifier] = true
	}
	for cat, idents := range activeByCategory {
		if err := ResolveCleared(ctx, a.pool, idents, cat); err != nil {
			a.logFn("ERROR", "analyzer: resolve cleared %s: %v", cat, err)
		}
	}
	a.logFn("INFO", "analyzer cycle: %d findings", len(findings))
}
