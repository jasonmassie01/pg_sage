package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
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
		event := notify.FindingCriticalEvent(f.Title, string(detail),
			a.databaseName, f.Category, f.ObjectIdentifier)
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

// finalizeCycle persists, notifies, publishes and resolves one cycle's
// deduplicated findings. evaluated names the categories whose producers
// completed successfully this cycle. A finding that fails to persist is
// still published unless suppressed. When the suppression state cannot be
// read, nothing is published, recorded or resolved this cycle (rules-06).
func (a *Analyzer) finalizeCycle(
	ctx context.Context, findings []Finding, evaluated map[string]bool,
) error {
	published, upsertErr, err := a.persistFindings(ctx, findings)
	if err != nil {
		a.logFn("ERROR", "analyzer: %v; keeping the previous %d findings, "+
			"skipping recommendations and resolving nothing this cycle",
			err, len(a.LatestFindings()))
		return fmt.Errorf("filter suppressed findings: %w", err)
	}
	a.mu.Lock()
	a.findings = published
	a.mu.Unlock()
	a.recordRecommendations(ctx, published, evaluated)
	a.resolveCleared(ctx, published, evaluated)
	a.resolveDisabled(ctx)
	a.logFn("INFO", "analyzer cycle: %d findings", len(published))
	return upsertErr
}

// persistFindings upserts findings, sends the notifications their upsert
// earned and returns the ones to publish: all but operator-suppressed
// identities. upsertErr is a persist failure (the rest still runs);
// filterErr means the suppression state could not be read, so nothing
// may be published, recorded or resolved.
func (a *Analyzer) persistFindings(ctx context.Context, findings []Finding) (
	published []Finding, upsertErr, filterErr error) {
	res, upsertErr := UpsertFindingsObserved(ctx, a.pool, findings, a.cycleEvidence())
	if upsertErr != nil {
		a.logFn("ERROR", "analyzer: %d of %d findings not persisted: %v",
			len(res.Failed), len(findings), upsertErr)
		upsertErr = fmt.Errorf("persist findings: %w", upsertErr)
	}
	// Notify only on new information: newly opened, reopened after an
	// action or severity-escalated findings, subject to a per-identity
	// cooldown (G2-B06/G7-B07). The upsert already skipped suppressed ones.
	a.dispatchCriticalFindings(ctx, a.notifiableCritical(res, time.Now()))
	a.dispatchFindingAlerts(ctx, res)
	a.dispatchRewriteFindings(ctx, res.Opened)

	// Operator-suppressed identities are never published to the executor,
	// the API or recommendations (G2-B01), whether or not the upsert
	// reached them.
	published, filterErr = withoutActiveSuppressions(ctx, a.suppressionReader(), findings)
	return published, upsertErr, filterErr
}

// cycleEvidence is when this cycle's snapshot was collected and how long
// after an action a persisting condition reopens the action's row.
func (a *Analyzer) cycleEvidence() Evidence {
	return Evidence{CollectedAt: a.eval.evidenceAt(), Window: reopenWindow(a.cfg)}
}

// suppressionReader is the source of the publish-time suppression filter.
func (a *Analyzer) suppressionReader() rowsQuerier {
	if a.suppressions != nil {
		return a.suppressions
	}
	return a.pool
}

// resolveCleared resolves open findings that are absent from this cycle's
// output, for every category in evaluated -- including categories that
// produced nothing, so the last finding of a category resolves (G2-B02).
// Categories not in evaluated (failed or skipped evaluators) are left
// untouched, and so are identities the cycle kept open (cycleEval.keep).
// Suppressed identities are absent from findings, so a legacy duplicate
// open row for them is resolved too.
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
		for ident := range a.eval.kept(cat) {
			idents[ident] = true
		}
		cleared, err := ResolveClearedFindings(ctx, a.pool, idents, cat)
		// Findings resolved before an error still get their resolve.
		a.dispatchResolvedCritical(ctx, cleared)
		a.dispatchFindingResolved(ctx, cleared)
		if err != nil {
			a.logFn("ERROR", "analyzer: resolve cleared %s: %v", cat, err)
		}
	}
}

// dispatchResolvedCritical sends the PagerDuty resolve for each cleared
// finding that is critical, or that this analyzer paged as critical
// before it was de-escalated (alerting-01). The event is a
// finding_critical on the page's key, so the rule that paged resolves.
// Resolving resets the identity's page cooldown: a re-open must page
// again, or PagerDuty would show an open critical finding as resolved.
func (a *Analyzer) dispatchResolvedCritical(
	ctx context.Context, cleared []ClearedFinding,
) {
	if a.dispatcher == nil {
		return
	}
	for _, c := range cleared {
		key := c.Category + "|" + c.ObjectIdentifier + "|critical"
		_, paged := a.notifiedAt[key]
		if c.Severity != "critical" && !paged {
			continue
		}
		delete(a.notifiedAt, key)
		event := notify.ResolvedEvent(notify.FindingCriticalEvent(c.Title,
			"no longer detected", a.databaseName, c.Category,
			c.ObjectIdentifier))
		if err := a.dispatcher.Dispatch(ctx, event); err != nil {
			a.logFn("ERROR", "critical finding resolve dispatch %s/%s: %v",
				c.Category, c.ObjectIdentifier, err)
		}
	}
}

// criticalNotifyCooldown bounds how often the same critical identity can
// be re-paged when it flaps between critical and a lower severity. A
// resolve clears it (dispatchResolvedCritical), so a finding re-opened
// after its page was resolved always pages again.
const criticalNotifyCooldown = time.Hour

// notifiableCritical returns the critical findings that were opened,
// reopened after an action or escalated this cycle and have not been notified within the cooldown.
// It is only called from the analyzer's cycle goroutine.
func (a *Analyzer) notifiableCritical(res UpsertResult, now time.Time) []Finding {
	if a.dispatcher == nil {
		return nil
	}
	candidates := append(append([]Finding(nil), res.Opened...), res.Reopened...)
	candidates = append(candidates, res.Escalated...)
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
