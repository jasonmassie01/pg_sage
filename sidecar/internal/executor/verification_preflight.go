package executor

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Up-front unverifiable (lab-01c): a step whose targets' hourly history
// says no watch can judge it within the hard max needs an operator's
// approval at every trust level, with the reason on the card. Classes
// judged immediately (VACUUM, ANALYZE, REINDEX) are exempt; no history is
// "unknown", written to the evidence, and never blocks.

// DetailVerificationPreflight is the finding detail holding the preflight.
const DetailVerificationPreflight = "verification_preflight"

// preflightReadTimeout bounds the hourly-history read.
const preflightReadTimeout = 5 * time.Second

// guardUnverifiable marks f approval_required when its step cannot be
// verified, and records the preflight in its detail. A failed read is
// logged and leaves f as it was (unknown: never inferred either way).
func (e *Executor) guardUnverifiable(ctx context.Context, f analyzer.Finding) analyzer.Finding {
	class := verificationClass(f.RecommendedSQL)
	targets := targetQueryIDs(f)
	if e.pool == nil || class == "" || verify.PreflightExempt(class) || len(targets) == 0 {
		return f
	}
	p, err := e.preflight(ctx, targets, time.Now())
	if err != nil {
		e.logFn("executor", "verification preflight of %q unavailable (not inferred): %v",
			f.Title, err)
		return f
	}
	f.Detail = maps.Clone(f.Detail)
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail[DetailVerificationPreflight] = p.Evidence()
	if p.Blocks() {
		return withApprovalReason(f, joinApprovalReason(f.Detail, p.Reason))
	}
	return f
}

// joinApprovalReason keeps a reason another guard already gave (an
// operator's rejection, a producer's marker) in front of reason, so the
// card names every cause of the approval.
func joinApprovalReason(detail map[string]any, reason string) string {
	prior, _ := detail[analyzer.DetailApprovalRequired].(string)
	switch prior = strings.TrimSpace(prior); {
	case prior == "":
		return reason
	case strings.Contains(prior, reason):
		return prior
	}
	return prior + "; " + reason
}

// preflight reads the targets' last PreflightWeeks weeks of hourly history
// and judges it against this executor's verification bounds.
func (e *Executor) preflight(ctx context.Context, targets []int64, now time.Time) (
	verify.Preflight, error) {
	options := verify.DefaultOptions()
	if e.cfg != nil {
		options = verificationOptions(e.cfg)
	}
	readCtx, cancel := context.WithTimeout(ctx, preflightReadTimeout)
	defer cancel()
	from := now.Add(-verify.PreflightWeeks * 7 * 24 * time.Hour)
	hourly, err := verify.NewCounterSource(e.pool).Hourly(readCtx, targets, from, now)
	if err != nil {
		return verify.Preflight{}, err
	}
	return verify.PreflightVerifiable(verify.PreflightInput{Targets: targets,
		Hourly: hourly, From: now, HardMax: options.HardMax, MinSamples: options.MinSamples})
}
