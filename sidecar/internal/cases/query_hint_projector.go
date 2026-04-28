package cases

import (
	"fmt"
	"time"
)

type SourceQueryHint struct {
	QueryID          int64
	DatabaseName     string
	HintText         string
	Symptom          string
	Status           string
	CreatedAt        time.Time
	BeforeCost       *float64
	AfterCost        *float64
	SuggestedRewrite string
	RewriteRationale string
	VerifiedAt       *time.Time
	RolledBackAt     *time.Time
}

func ProjectQueryHint(h SourceQueryHint) Case {
	c := NewCase(CaseInput{
		SourceType:   SourceQueryType,
		SourceID:     fmt.Sprintf("%d", h.QueryID),
		DatabaseName: h.DatabaseName,
		IdentityKey:  queryHintIdentityKey(h),
		Title:        queryHintTitle(h),
		Severity:     queryHintSeverity(h),
		Why:          queryHintWhy(h),
		WhyNow:       queryHintWhyNow(h),
		Evidence:     queryHintEvidence(h),
		ObservedAt:   h.CreatedAt,
	})
	if h.Status == "retired" {
		c.State = StateResolved
	}
	if h.VerifiedAt != nil {
		c.UpdatedAt = *h.VerifiedAt
	}
	if h.RolledBackAt != nil {
		c.UpdatedAt = *h.RolledBackAt
	}
	return c
}

func queryHintIdentityKey(h SourceQueryHint) string {
	return fmt.Sprintf("query_hint:%s:%d:%s", h.DatabaseName,
		h.QueryID, h.HintText)
}

func queryHintTitle(h SourceQueryHint) string {
	status := h.Status
	if status == "" {
		status = "active"
	}
	return fmt.Sprintf("Query hint %s for query %d", status, h.QueryID)
}

func queryHintSeverity(h SourceQueryHint) Severity {
	if h.Status == "broken" {
		return SeverityWarning
	}
	return SeverityInfo
}

func queryHintWhy(h SourceQueryHint) string {
	if h.Symptom != "" {
		return h.Symptom
	}
	return "Query planner hint is being tracked for verification."
}

func queryHintWhyNow(h SourceQueryHint) string {
	switch h.Status {
	case "active":
		return "hint is active and needs verification"
	case "broken":
		return "hint broke and needs cleanup or replacement"
	case "retired":
		return "hint retired after revalidation"
	default:
		return "query tuning experiment needs review"
	}
}

func queryHintEvidence(h SourceQueryHint) []Evidence {
	detail := map[string]any{
		"queryid":   h.QueryID,
		"hint_text": h.HintText,
		"status":    h.Status,
	}
	if h.BeforeCost != nil {
		detail["before_cost"] = *h.BeforeCost
	}
	if h.AfterCost != nil {
		detail["after_cost"] = *h.AfterCost
	}
	if h.SuggestedRewrite != "" {
		detail["suggested_rewrite"] = h.SuggestedRewrite
		detail["rewrite_rationale"] = h.RewriteRationale
	}
	return []Evidence{{
		Type:    "query_hint",
		Summary: queryHintWhy(h),
		Detail:  detail,
	}}
}
