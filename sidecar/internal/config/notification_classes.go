package config

import (
	"fmt"
	"slices"
)

// Notification classes and the chat approval cap (ux-09a, decision D7).
// Both keys live in notification_policy, which is YAML-only and
// restart-bound: the cap is a safety boundary, so neither the API nor a
// stored override can raise it.

// The values of notification_policy.chat_approval_max_tier. There is no
// "high": a high-risk change is approved in pg_sage, never from chat.
const (
	ChatApprovalCapSafe     = "safe"
	ChatApprovalCapModerate = "moderate"
	ChatApprovalCapNone     = "none"
)

// ChatApprovalCaps lists the allowed caps, lowest first.
var ChatApprovalCaps = []string{ChatApprovalCapNone, ChatApprovalCapSafe,
	ChatApprovalCapModerate}

const (
	// DefaultNotificationPolicyAskBatchMinutes batches one database's Ask
	// notifications into one message per 15 minutes.
	DefaultNotificationPolicyAskBatchMinutes = 15
	// maxAskBatchMinutes bounds the batch window to a day.
	maxAskBatchMinutes = 1440
)

func (p NotificationPolicyConfig) validate() error {
	if !slices.Contains(ChatApprovalCaps, p.ChatApprovalMaxTier) {
		return fmt.Errorf("notification_policy.chat_approval_max_tier %q is not allowed: "+
			"use safe, moderate or none (high-risk changes are approved in pg_sage)",
			p.ChatApprovalMaxTier)
	}
	if p.AskBatchMinutes < 0 || p.AskBatchMinutes > maxAskBatchMinutes {
		return fmt.Errorf("notification_policy.ask_batch_minutes %d is out of range 0-%d",
			p.AskBatchMinutes, maxAskBatchMinutes)
	}
	return nil
}
