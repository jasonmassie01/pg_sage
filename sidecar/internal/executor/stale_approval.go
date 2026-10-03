package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// Dogfood lifeos (stale approval): an optimizer CREATE INDEX was queued for
// approval while its HypoPG what-if was unverified. The optimizer later
// re-verified it, but the gate read the recommendation revision's evidence,
// which is immutable and outside the content hash, so it kept answering
// queue_approval and the proposal stayed pending until it expired. The
// gate now reads the open finding's current evidence, and when it then
// authorizes the change, the pending proposal for exactly that content is
// superseded under the change lease right before the change runs.

// gateEvidenceKeys are the finding detail keys the standing-gate request
// depends on (the what-if verdict and the producer's approval marker).
var gateEvidenceKeys = []string{
	"what_if_verdict", "what_if_reason", "hypopg_validated", analyzer.DetailApprovalRequired,
}

// currentGateEvidence returns f with the gate evidence of its open finding
// (the analyzer rewrites it every cycle) when that finding still proposes
// f's SQL, and with the approval guardrail when an operator rejected this
// exact change. Failing to read either keeps the safer view.
func (e *Executor) currentGateEvidence(
	ctx context.Context, f analyzer.Finding, findingID int64,
	cand *recommendation.Candidate,
) analyzer.Finding {
	var sql string
	var raw []byte
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(recommended_sql, ''),
		detail FROM sage.findings WHERE id = $1 AND status = 'open'`,
		findingID).Scan(&sql, &raw)
	var live map[string]any
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		e.logFn("executor", "read current evidence of %q: %v", f.Title, err)
	case strings.TrimSpace(sql) != strings.TrimSpace(f.RecommendedSQL):
		// The finding moved on: its verdict is about other SQL.
	case json.Unmarshal(raw, &live) != nil:
		e.logFn("executor", "decode current evidence of %q: invalid JSON", f.Title)
	default:
		f.Detail = overlayGateEvidence(f.Detail, live)
	}
	queueID, hold, err := e.operatorHold(ctx, f, findingID, cand)
	switch {
	case err != nil:
		e.logFn("executor", "read operator decisions on %q: %v", f.Title, err)
		return withApprovalReason(f, "operator decisions on this change could not be read")
	case hold == "rejected":
		return requireApprovalAfterRejection(f, queueID)
	case hold == "snoozed":
		return withApprovalReason(f, fmt.Sprintf(
			"an operator snoozed this exact change (queue item %d)", queueID))
	}
	return f
}

// overlayGateEvidence copies detail with every gate key taken from live:
// set where live has it, removed where it no longer does.
func overlayGateEvidence(detail, live map[string]any) map[string]any {
	out := maps.Clone(detail)
	if out == nil {
		out = map[string]any{}
	}
	for _, key := range gateEvidenceKeys {
		if value, ok := live[key]; ok {
			out[key] = value
		} else {
			delete(out, key)
		}
	}
	return out
}

// requireApprovalAfterRejection keeps a change an operator rejected behind
// approval: autonomy never overrides a human "no" (the queue path's
// re-proposal rules still apply).
func requireApprovalAfterRejection(f analyzer.Finding, queueID int) analyzer.Finding {
	return withApprovalReason(f, fmt.Sprintf(
		"an operator rejected this exact change (queue item %d)", queueID))
}

func withApprovalReason(f analyzer.Finding, reason string) analyzer.Finding {
	f.Detail = maps.Clone(f.Detail)
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail[analyzer.DetailApprovalRequired] = reason
	return f
}

// operatorHold reports an operator's "no" (rejected) or "not now" (a running
// snooze of a pending proposal) on exactly f's content, with its queue item.
func (e *Executor) operatorHold(
	ctx context.Context, f analyzer.Finding, findingID int64,
	cand *recommendation.Candidate,
) (int, string, error) {
	rows, err := loadQueuedApprovals(ctx, e.pool, []string{"rejected", "pending"},
		findingID, f.RecommendedSQL, cand, false)
	if err != nil {
		return 0, "", err
	}
	for _, q := range rows {
		if !q.sameContent(f.RecommendedSQL, cand) {
			continue
		}
		if q.Status == "rejected" {
			return q.ID, "rejected", nil
		}
		if q.Snoozed && !q.Expired {
			return q.ID, "snoozed", nil
		}
	}
	return 0, "", nil
}

// whatIfVerified reports a HypoPG what-if verdict of verified.
func whatIfVerified(detail map[string]any) bool {
	verdict, _ := detail["what_if_verdict"].(string)
	return verdict == optimizer.WhatIfVerified
}

// staleApprovalReason is the queue reason of a superseded proposal.
func staleApprovalReason(
	f analyzer.Finding, cand *recommendation.Candidate, decisionID int64,
) string {
	why := "the standing policy now authorizes it"
	if cand != nil && whatIfVerified(f.Detail) && !whatIfVerified(cand.Current.Evidence) {
		why = "what-if verified"
	}
	reason := "approval no longer required: " + why
	if decisionID > 0 {
		reason += fmt.Sprintf(" (decision %d)", decisionID)
	}
	return reason
}
