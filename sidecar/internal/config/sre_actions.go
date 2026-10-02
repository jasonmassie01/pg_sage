package config

import (
	"fmt"
	"strings"
	"time"
)

// SREActionsConfig configures Sage SRE approved actions (M5): a concluded
// investigation whose root is one active backend proposes an
// evidence-matched cancel. Proposals are on by default; execution always
// needs a human approval (there is no automatic execution in M5).
type SREActionsConfig struct {
	Proposals               bool     `yaml:"proposals" doc:"Propose an evidence-matched cancel when an investigation's root is one active blocking backend. Proposals never execute by themselves. Default: true."`
	RequestApproval         bool     `yaml:"request_approval" doc:"Queue an approved-to-run proposal in the approval queue and send it to ChatOps channels at once. Default: true."`
	ApprovalTTLMinutes      int      `yaml:"approval_ttl_minutes" doc:"Minutes an action approval request stays open before it expires, 1-60. Default: 15."`
	MaxEvidenceAgeSeconds   int      `yaml:"max_evidence_age_seconds" doc:"Oldest target identity evidence an approved cancel may act on, 1-5 seconds (5 is the ceiling). Default: 5."`
	RecoverySampleSeconds   int      `yaml:"recovery_sample_seconds" doc:"Seconds between recovery samples after an action, 1-300. Default: 40."`
	RecoverySamples         int      `yaml:"recovery_samples" doc:"Fresh samples a recovery verdict needs, 3-20. Default: 3."`
	RecoveryDeadlineMinutes int      `yaml:"recovery_deadline_minutes" doc:"Minutes after an action before an unproven recovery is reported inconclusive, 1-240. Default: 30."`
	ChatOpsToleranceSeconds int      `yaml:"chatops_tolerance_seconds" doc:"Maximum age (and clock skew) of a signed Slack approval callback, 30-900 seconds. Default: 300."`
	ProtectedRoles          []string `yaml:"protected_roles" doc:"Database roles whose sessions are never proposed for or signalled by an action (exact names)."`
	ProtectedApplications   []string `yaml:"protected_applications" doc:"application_name values whose sessions are never proposed for or signalled by an action (exact names)."`
}

// Sage SRE action defaults.
const (
	DefaultSREApprovalTTLMinutes      = 15
	DefaultSREMaxEvidenceAgeSeconds   = 5
	DefaultSRERecoverySampleSeconds   = 40
	DefaultSRERecoverySamples         = 3
	DefaultSRERecoveryDeadlineMinutes = 30
	DefaultSREChatOpsToleranceSeconds = 300
)

func defaultSREActionsConfig() SREActionsConfig {
	return SREActionsConfig{Proposals: true, RequestApproval: true,
		ApprovalTTLMinutes:      DefaultSREApprovalTTLMinutes,
		MaxEvidenceAgeSeconds:   DefaultSREMaxEvidenceAgeSeconds,
		RecoverySampleSeconds:   DefaultSRERecoverySampleSeconds,
		RecoverySamples:         DefaultSRERecoverySamples,
		RecoveryDeadlineMinutes: DefaultSRERecoveryDeadlineMinutes,
		ChatOpsToleranceSeconds: DefaultSREChatOpsToleranceSeconds}
}

// ApprovalTTL is how long an approval request stays open.
func (a SREActionsConfig) ApprovalTTL() time.Duration {
	return time.Duration(a.ApprovalTTLMinutes) * time.Minute
}

// MaxEvidenceAge is the oldest identity evidence a cancel may act on.
func (a SREActionsConfig) MaxEvidenceAge() time.Duration {
	return time.Duration(a.MaxEvidenceAgeSeconds) * time.Second
}

// RecoveryInterval is the gap between recovery samples.
func (a SREActionsConfig) RecoveryInterval() time.Duration {
	return time.Duration(a.RecoverySampleSeconds) * time.Second
}

// RecoveryDeadline bounds a recovery observation.
func (a SREActionsConfig) RecoveryDeadline() time.Duration {
	return time.Duration(a.RecoveryDeadlineMinutes) * time.Minute
}

// ChatOpsTolerance is the accepted age of a signed callback.
func (a SREActionsConfig) ChatOpsTolerance() time.Duration {
	return time.Duration(a.ChatOpsToleranceSeconds) * time.Second
}

func (a SREActionsConfig) validate() error {
	for _, r := range []struct {
		key           string
		value, lo, hi int
	}{
		{"approval_ttl_minutes", a.ApprovalTTLMinutes, 1, 60},
		{"max_evidence_age_seconds", a.MaxEvidenceAgeSeconds, 1, 5},
		{"recovery_sample_seconds", a.RecoverySampleSeconds, 1, 300},
		{"recovery_samples", a.RecoverySamples, 3, 20},
		{"recovery_deadline_minutes", a.RecoveryDeadlineMinutes, 1, 240},
		{"chatops_tolerance_seconds", a.ChatOpsToleranceSeconds, 30, 900},
	} {
		if r.value < r.lo || r.value > r.hi {
			return fmt.Errorf("sre.actions.%s must be %d-%d, got %d", r.key, r.lo, r.hi,
				r.value)
		}
	}
	if span := time.Duration(a.RecoverySamples) * a.RecoveryInterval(); span >
		a.RecoveryDeadline() {
		return fmt.Errorf("sre.actions.recovery_deadline_minutes (%d) is shorter than "+
			"%d samples every %d s", a.RecoveryDeadlineMinutes, a.RecoverySamples,
			a.RecoverySampleSeconds)
	}
	for key, names := range map[string][]string{"protected_roles": a.ProtectedRoles,
		"protected_applications": a.ProtectedApplications} {
		for _, n := range names {
			if strings.TrimSpace(n) == "" {
				return fmt.Errorf("sre.actions.%s has a blank name", key)
			}
		}
	}
	return nil
}
