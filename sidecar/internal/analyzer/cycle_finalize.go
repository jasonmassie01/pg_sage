package analyzer

import (
	"context"
	"encoding/json"
	"time"

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
	res, err := UpsertFindingsWithResult(ctx, a.pool, findings)
	if err != nil {
		a.logFn("ERROR", "analyzer: upsert findings: %v", err)
	}
	// Operator-suppressed identities are never published to the executor
	// or notifications (G2-B01).
	findings = withoutSuppressed(findings, res)
	a.mu.Lock()
	a.findings = findings
	a.mu.Unlock()
	// Notify only on new information: newly opened or severity-escalated
	// findings, subject to a per-identity cooldown (G2-B06/G7-B07).
	a.dispatchCriticalFindings(ctx, a.notifiableCritical(res, time.Now()))
	a.dispatchRewriteFindings(ctx, res.Opened)

	a.resolveCleared(ctx, findings, evaluated)
	a.logFn("INFO", "analyzer cycle: %d findings", len(findings))
}

// resolveCleared resolves open findings that are absent from this cycle's
// output, for every category in evaluated -- including categories that
// produced nothing, so the last finding of a category resolves (G2-B02).
// Categories not in evaluated (failed or skipped evaluators) are left
// untouched. Suppressed identities are absent from findings, so a legacy
// duplicate open row for them is resolved too.
func (a *Analyzer) resolveCleared(
	ctx context.Context, findings []Finding, evaluated map[string]bool,
) {
	activeByCategory := make(map[string]map[string]bool, len(evaluated))
	for cat := range evaluated {
		activeByCategory[cat] = make(map[string]bool)
	}
	for _, f := range findings {
		if idents, ok := activeByCategory[f.Category]; ok {
			idents[f.ObjectIdentifier] = true
		}
	}
	for cat, idents := range activeByCategory {
		if err := ResolveCleared(ctx, a.pool, idents, cat); err != nil {
			a.logFn("ERROR", "analyzer: resolve cleared %s: %v", cat, err)
		}
	}
}

// withoutSuppressed drops findings whose identity is under an active
// operator suppression.
func withoutSuppressed(findings []Finding, res UpsertResult) []Finding {
	if len(res.Suppressed) == 0 {
		return findings
	}
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if !res.IsSuppressed(f) {
			out = append(out, f)
		}
	}
	return out
}

// criticalNotifyCooldown bounds how often the same critical identity can
// be re-paged when it flaps between resolved and re-opened.
const criticalNotifyCooldown = time.Hour

// notifiableCritical returns the critical findings that were opened or
// escalated this cycle and have not been notified within the cooldown.
// It is only called from the analyzer's cycle goroutine.
func (a *Analyzer) notifiableCritical(res UpsertResult, now time.Time) []Finding {
	if a.dispatcher == nil {
		return nil
	}
	candidates := append(append([]Finding(nil), res.Opened...), res.Escalated...)
	var out []Finding
	for _, f := range candidates {
		if f.Severity != "critical" {
			continue
		}
		key := f.Category + "|" + f.ObjectIdentifier + "|" + f.Severity
		if last, ok := a.notifiedAt[key]; ok && now.Sub(last) < criticalNotifyCooldown {
			continue
		}
		if a.notifiedAt == nil {
			a.notifiedAt = make(map[string]time.Time)
		}
		a.notifiedAt[key] = now
		out = append(out, f)
	}
	return out
}
