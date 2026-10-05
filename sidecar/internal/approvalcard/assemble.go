package approvalcard

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// Assemble builds a card from its inputs. It reads nothing; Loader reads
// the inputs from the database.
func Assemble(in Inputs) Card {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	a := in.Action
	c := Card{QueueID: a.ID, Database: in.Database, ActionType: actionTypeOf(a),
		Status: a.Status, SQL: a.ProposedSQL, ProposedAt: a.ProposedAt,
		ExpiresAt: a.ExpiresAt, CardHash: ContentHash(a)}
	c.Title = cardTitle(in, c.ActionType)
	c.Finding = findingRef(in.Finding)
	c.Recommendation = recommendationRef(a)
	c.Targets = targetsOf(in.Finding)
	c.Evidence = evidenceOf(in)
	c.Rationale = rationaleOf(in)
	c.Predicted = predictedOf(in)
	c.Rollback = rollbackOf(a, in.Contract)
	c.Risk = riskOf(in, c)
	c.VerificationWait = waitOf(in, now)
	c.Why = append(whyOf(in, c.ActionType), waitReason(c.VerificationWait)...)
	c.Trust = trustOf(in)
	c.Origin = originOf(a)
	if s := in.Snooze; s != nil && s.Until.After(now) {
		until := s.Until
		c.SnoozedUntil, c.SnoozeReason = &until, s.Reason
	}
	return c
}

// actionTypeOf is the row's action type, or the one its SQL implies for
// rows queued before action types were recorded.
func actionTypeOf(a store.QueuedAction) string {
	if t := strings.TrimSpace(a.ActionType); t != "" {
		return t
	}
	sql := strings.ToUpper(strings.TrimSpace(a.ProposedSQL))
	for _, p := range []struct{ prefix, typ string }{
		{"ANALYZE ", "analyze_table"},
		{"CREATE INDEX CONCURRENTLY ", "create_index_concurrently"},
		{"DROP INDEX ", "drop_unused_index"},
		{"ALTER TABLE ", "alter_table"},
		{"ALTER SYSTEM ", "alter_system_guc"},
		{"VACUUM ", "vacuum_table"},
		{"REINDEX ", "reindex_concurrently"},
	} {
		if strings.HasPrefix(sql, p.prefix) {
			return p.typ
		}
	}
	return ""
}

func cardTitle(in Inputs, actionType string) string {
	if in.Revision != nil && strings.TrimSpace(in.Revision.Title) != "" {
		return in.Revision.Title
	}
	if in.Finding != nil && strings.TrimSpace(in.Finding.Title) != "" {
		return in.Finding.Title
	}
	name := strings.ReplaceAll(actionType, "_", " ")
	if name == "" {
		name = "action"
	}
	return fmt.Sprintf("%s%s (queue item %d)", strings.ToUpper(name[:1]), name[1:],
		in.Action.ID)
}

func findingRef(f *FindingRow) *FindingRef {
	if f == nil {
		return nil
	}
	return &FindingRef{ID: f.ID, Category: f.Category, Severity: f.Severity,
		Object: f.Object, Title: f.Title, Recommendation: f.Recommendation}
}

func recommendationRef(a store.QueuedAction) *RecommendationRef {
	if a.RecommendationID == nil {
		return nil
	}
	ref := &RecommendationRef{ID: *a.RecommendationID, ContentHash: a.ContentHash}
	if a.RecommendationRevision != nil {
		ref.Revision = *a.RecommendationRevision
	}
	return ref
}

// targetsOf names the objects the action changes: the finding's table and
// object (an optimizer identity "table|definition" counts as its table).
func targetsOf(f *FindingRow) []string {
	out := []string{}
	if f == nil {
		return out
	}
	add := func(s string) {
		s = strings.TrimSpace(s)
		for _, have := range out {
			if have == s {
				return
			}
		}
		if s != "" {
			out = append(out, s)
		}
	}
	if table, ok := f.Detail["table"].(string); ok {
		add(table)
	}
	object, _, _ := strings.Cut(f.Object, "|")
	add(object)
	return out
}

// rationaleOf is the model's rationale when a model wrote one, else the
// rule's recommendation.
func rationaleOf(in Inputs) *Rationale {
	detail := map[string]any{}
	recommendation := ""
	if in.Finding != nil {
		detail, recommendation = in.Finding.Detail, in.Finding.Recommendation
	}
	for _, key := range []string{"llm_rationale", "rationale", "narrative",
		"rewrite_rationale"} {
		if text, ok := detail[key].(string); ok && strings.TrimSpace(text) != "" {
			r := &Rationale{Source: "llm", Text: truncate(text, 1200)}
			if conf, ok := number(detail["confidence_score"]); ok {
				r.Confidence = &conf
			}
			r.Calibration = calibrationText(detail["confidence_calibration"])
			return r
		}
	}
	if recommendation == "" && in.Revision != nil {
		recommendation = in.Revision.Recommendation
	}
	if strings.TrimSpace(recommendation) == "" {
		return nil
	}
	return &Rationale{Source: "rule", Text: truncate(recommendation, 1200)}
}

// predictedOf reads the predicted effect the producer recorded.
func predictedOf(in Inputs) Predicted {
	detail := map[string]any{}
	if in.Finding != nil && in.Finding.Detail != nil {
		detail = in.Finding.Detail
	} else if in.Revision != nil && in.Revision.Evidence != nil {
		detail = in.Revision.Evidence
	}
	var p Predicted
	if pct, ok := number(detail["estimated_improvement_pct"]); ok {
		p.ImprovementPct = &pct
	}
	if size, ok := number(detail["estimated_size_bytes"]); ok && size > 0 {
		bytes := int64(size)
		p.EstimatedSizeBytes = &bytes
	}
	p.AffectedQueries = queryTexts(detail["affected_queries"])
	p.QueryIDs = queryIDs(detail["queryids"])
	p.WhatIfVerdict, _ = detail["what_if_verdict"].(string)
	p.WhatIfReason, _ = detail["what_if_reason"].(string)
	validated, _ := detail["hypopg_validated"].(bool)
	switch {
	case p.WhatIfVerdict == "verified" || validated:
		p.Method = "hypopg"
	case p.ImprovementPct != nil:
		p.Method = "llm_estimate"
	}
	return p
}

func queryTexts(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, q := range list {
		if s, ok := q.(string); ok && strings.TrimSpace(s) != "" && len(out) < maxQueries {
			out = append(out, truncate(s, 300))
		}
	}
	return out
}

func queryIDs(v any) []int64 {
	var out []int64
	switch list := v.(type) {
	case []any:
		for _, q := range list {
			if n, ok := number(q); ok {
				out = append(out, int64(n))
			}
		}
	case []int64:
		out = append(out, list...)
	}
	return out
}
