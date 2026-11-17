package notify

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EventDigestReady is the scheduled digest (ux-02b): what pg_sage did,
// verified, saved, what needs a person and what it could not do. It is
// sent through the rules like every event, so each channel kind gets it.
const EventDigestReady = "digest_ready"

// Channel outcomes of one Submit, in ChannelOutcome.Status.
const (
	OutcomeSent    = "sent"    // delivered at once
	OutcomeError   = "error"   // delivered at once and the send failed
	OutcomeQueued  = "queued"  // waiting in the outbox
	OutcomeRefused = "refused" // could not be delivered or queued
	OutcomeHeld    = "held"    // the alert state decided not to send now
)

// ChannelOutcome is what one matching rule's channel did with an event.
// Error is redacted, as in the notification log.
type ChannelOutcome struct {
	ChannelID int    `json:"channel_id"`
	Channel   string `json:"channel"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// syncEvents are delivered at once even with the outbox: the digest's row
// records each channel's real outcome, and a digest is sent once per
// schedule, never retried into a later one.
var syncEvents = map[string]bool{EventDigestReady: true}

// queuedOutcome names what enqueue did, from the counters it moved.
func queuedOutcome(before, after DispatchResult) string {
	switch {
	case after.Enqueued > before.Enqueued:
		return OutcomeQueued
	case after.Suppressed > before.Suppressed:
		return OutcomeHeld
	}
	return OutcomeRefused
}

// digestRuleExcludedTypes never get a default digest rule: a daily
// summary must not page anyone.
var digestRuleExcludedTypes = []string{"pagerduty", "opsgenie", "incidentio"}

// ensureDefaultRuleSQL adds an info rule for $1 on the first enabled
// channel that suits a digest, unless any rule for $1 exists (an owner's
// disabled rule included). The advisory lock keeps racing callers to one.
const ensureDefaultRuleSQL = `/* pg_sage */
INSERT INTO sage.notification_rules (channel_id, event, min_severity, enabled)
SELECT c.id, $1, 'info', true FROM sage.notification_channels c
WHERE c.enabled AND c.type <> ALL ($2::text[])
  AND NOT EXISTS (SELECT 1 FROM sage.notification_rules r WHERE r.event = $1)
ORDER BY c.id LIMIT 1`

// EnsureDefaultRule creates the default rule for event when none exists
// and reports whether it did (ux-02b). Only digest_ready has one.
func (s *PoolStore) EnsureDefaultRule(ctx context.Context, event string) (bool, error) {
	if s == nil || s.pool == nil {
		return false, errors.New("ensure default notification rule: notification pool is nil")
	}
	if event != EventDigestReady {
		return false, fmt.Errorf("ensure default notification rule: event %q has no "+
			"default rule (only %s does)", event, EventDigestReady)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return false, fmt.Errorf("ensure default %s rule: begin: %w", event, err)
	}
	defer func() { _ = tx.Rollback(qctx) }()
	if _, err := tx.Exec(qctx, `/* pg_sage */ SELECT pg_advisory_xact_lock(
		hashtext('pg_sage.notify.default_rule'))`); err != nil {
		return false, fmt.Errorf("ensure default %s rule: lock: %w", event, err)
	}
	tag, err := tx.Exec(qctx, ensureDefaultRuleSQL, event, digestRuleExcludedTypes)
	if err != nil {
		return false, fmt.Errorf("ensure default %s rule: insert: %w", event, err)
	}
	if err := tx.Commit(qctx); err != nil {
		return false, fmt.Errorf("ensure default %s rule: commit: %w", event, err)
	}
	return tag.RowsAffected() == 1, nil
}
