package main

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/notify"
)

// NotifyRootAuthority tells an operator, through the database's
// notification rules (a grant as an executed action, a loss as a failed
// one, like trust promotions and demotions) and the log, that a family
// earned or lost model-root authority, and which report decided it
// (roadmap 2.4 owner addition A).
func (n autonomyNotifier) NotifyRootAuthority(ctx context.Context,
	c earned.RootAuthorityChange) error {
	report := c.ReportID
	if report == "" {
		report = "none (no measurement counts for this build)"
	}
	typ, state, subject := "action_executed", "granted", fmt.Sprintf(
		"pg_sage model may now override the causal graph's root: %s on %s", c.Family,
		c.Database)
	if !c.Granted {
		typ, state, subject = "action_failed", "revoked", fmt.Sprintf(
			"pg_sage model roots are advisory (L1) again: %s on %s", c.Family, c.Database)
	}
	logWarn("autonomy", "%s (report %s): %s", subject, report, c.Reason)
	if n.dispatcher == nil {
		return nil
	}
	return n.dispatcher.Dispatch(ctx, notify.Event{Type: typ, Severity: "warning",
		Subject: subject,
		Body: fmt.Sprintf("Database: %s\nFamily: %s\nModel-root authority: %s\n"+
			"Report: %s\nWhy: %s", c.Database, c.Family, state, report, c.Reason),
		Data: map[string]any{"database": c.Database, "family": string(c.Family),
			"granted": c.Granted, "report_id": c.ReportID, "reason": c.Reason},
		DedupKey: fmt.Sprintf("model-root:%s:%s:%s:%s:%d", c.Database, c.Family, state,
			c.ReportID, c.At.Unix())})
}
