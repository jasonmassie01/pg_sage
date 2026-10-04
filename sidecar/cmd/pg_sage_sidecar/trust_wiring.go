package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/notify"
)

// One trust system (roadmap 1.2) wiring: the class promotion bar, the
// startup explanation of what the trust settings mean now, the one-time
// grandfathering log, and demotion notifications through the database's
// notification rules.

// classPromotionThresholds maps sre.autonomy.class_promotion onto the
// ledger's self-initiated promotion bar.
func classPromotionThresholds(th earned.Thresholds,
	p config.SREClassPromotionConfig) earned.Thresholds {
	th.ClassMinSuccessesL2, th.ClassMinSuccessesL3 = p.MinSuccessesL2, p.MinSuccessesL3
	th.ClassMinSuccessRate = p.MinSuccessRatePct / 100
	return th.Normalized()
}

// logTrustMeaning says, once at startup, what the existing trust settings
// mean under the unified ledger, with the configured values.
func logTrustMeaning(c *config.Config, info, warn func(component, format string,
	args ...any)) {
	t, a := c.Trust, c.SRE.Autonomy
	info("startup", "TRUST: the ledger grants, the operator caps. The trust ledger "+
		"decides each action class's level per database (L1 script, L2 one-click "+
		"approval, L3 unattended) from verified outcomes, and an admin approves every "+
		"promotion (see /api/v1/trust and the Trust page). trust.level=%s, "+
		"trust.tier3_safe=%t and trust.tier3_moderate=%t are the operator's ceiling "+
		"and kill switch, a permanent part of the design: the ledger never grants "+
		"past them, and lowering them takes autonomy back at once.", t.Level,
		t.Tier3Safe, t.Tier3Moderate)
	info("startup", "TRUST: trust.ramp_safe_hours=%d and trust.ramp_moderate_hours=%d are "+
		"now a floor: the minimum observation before pg_sage may propose a promotion "+
		"(L2, and L3 of SAFE classes / L3 of MODERATE classes). They never grant "+
		"autonomy on their own.", t.RampSafeHours, t.RampModerateHours)
	info("startup", "TRUST: autonomy the ramp had already granted is kept per database as "+
		"grandfathered levels on the first start under the ledger; they demote one "+
		"level on a regressed verdict, an operator rollback or a rejection, like any "+
		"other (class promotion bar: %d / %d successes, %.0f%%).",
		a.ClassPromotion.MinSuccessesL2, a.ClassPromotion.MinSuccessesL3,
		a.ClassPromotion.MinSuccessRatePct)
	if !a.Enforce {
		warn("startup", "TRUST: sre.autonomy.enforce=false: the ledger is not consulted and "+
			"the elapsed-time ramp still grants autonomy (legacy behaviour); set it to "+
			"true to let earned evidence decide")
	}
}

// logGrandfathered logs a database's grandfathering when this start did
// it; nothing afterwards.
func logGrandfathered(rep earned.GrandfatherReport, info func(component, format string,
	args ...any)) {
	if !rep.Migrated {
		return
	}
	pairs := make([]string, 0, len(rep.Seeded))
	for _, st := range rep.Seeded {
		pairs = append(pairs, fmt.Sprintf("%s/%s=%s", st.Family, st.Class, st.Level))
	}
	sort.Strings(pairs)
	if len(pairs) == 0 {
		pairs = append(pairs, "none (nothing ran unattended under the ramp)")
	}
	info("autonomy", "db %q: trust ledger took over; grandfathered from the time ramp: %s",
		rep.Database, strings.Join(pairs, ", "))
}

// NotifyDemotion tells an operator, through the database's notification
// rules for failed actions, that a class lost a level and why.
func (n autonomyNotifier) NotifyDemotion(ctx context.Context, d earned.Demotion) error {
	subject := fmt.Sprintf("pg_sage trust demoted: %s/%s on %s %s -> %s (%s)", d.Family,
		d.Class, d.Database, d.From, d.To, d.Cause)
	logWarn("autonomy", "%s: %s", subject, d.Detail)
	if n.dispatcher == nil {
		return nil
	}
	return n.dispatcher.Dispatch(ctx, notify.Event{Type: "action_failed",
		Severity: "warning", Subject: subject,
		Body: fmt.Sprintf("Database: %s\nClass: %s/%s\nLevel: %s -> %s\nCause: %s\n%s",
			d.Database, d.Family, d.Class, d.From, d.To, d.Cause, d.Detail),
		Data: map[string]any{"database": d.Database, "family": string(d.Family),
			"class": string(d.Class), "from": d.From.String(), "to": d.To.String(),
			"cause": d.Cause, "action_log_id": d.ActionLogID, "queue_id": d.QueueID},
		DedupKey: fmt.Sprintf("trust-demotion:%s:%s:%s:%d:%d", d.Database, d.Family,
			d.Class, d.ActionLogID, d.QueueID)})
}
