package executor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/verify"
)

// executor-03: the outcome text of an index verdict says that the write
// signal is database-wide and advisory, so an operator reading a retained
// index next to a write surge sees why it was kept.
// No concurrent-access tests: outcomeReason is a pure function of a verdict.

func signalVerdict(signal map[string]any) verify.Verdict {
	return verify.Verdict{Retain: true, Status: "success", Evidence: map[string]any{
		"comparison":   map[string]any{"reason": "gain_confirmed"},
		"write_signal": signal,
	}}
}

func TestOutcomeReasonNamesAdvisoryWriteSurge(t *testing.T) {
	got := outcomeReason(signalVerdict(map[string]any{
		"scope": "database", "metric": "blk_write_time", "delta_pct": 50.0,
		"threshold_pct": 20.0, "exceeds_threshold": true, "decisive": false,
	}))
	for _, want := range []string{"gain_confirmed", "database-wide", "advisory",
		"+50.0%", "20%"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reason = %q, want it to contain %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "gain_confirmed; ") {
		t.Fatalf("reason = %q, want the query reason first", got)
	}
}

func TestOutcomeReasonOmitsSignalUnderThreshold(t *testing.T) {
	cases := map[string]map[string]any{
		"under threshold": {"delta_pct": 10.0, "threshold_pct": 20.0,
			"exceeds_threshold": false},
		"read error":   {"error": "read post-action write measurements: boom"},
		"no baseline":  {"unavailable": "no pre-action write baseline"},
		"wrong type":   {"exceeds_threshold": "true"},
		"empty signal": {},
	}
	for name, signal := range cases {
		if got := outcomeReason(signalVerdict(signal)); got != "gain_confirmed" {
			t.Fatalf("%s: reason = %q, want just the query reason", name, got)
		}
	}
}

func TestOutcomeReasonKeepsRevertReasonAndStatusFallback(t *testing.T) {
	revert := verify.Verdict{Revert: true, Status: "reverted", Reason: "no_gain",
		Evidence: map[string]any{"comparison": map[string]any{"reason": "below_gain"},
			"write_signal": map[string]any{"delta_pct": 80.0, "threshold_pct": 20.0,
				"exceeds_threshold": true}}}
	got := outcomeReason(revert)
	if !strings.HasPrefix(got, "no_gain: below_gain; ") || !strings.Contains(got, "advisory") {
		t.Fatalf("revert reason = %q, want no_gain: below_gain then the advisory note", got)
	}
	// Nil evidence (an invalid-index revert) and an empty verdict.
	if got := outcomeReason(verify.Verdict{Status: "reverted", Reason: "invalid_index"}); got !=
		"invalid_index" {
		t.Fatalf("invalid-index reason = %q", got)
	}
	if got := outcomeReason(verify.Verdict{Status: "unverifiable"}); got != "unverifiable" {
		t.Fatalf("empty verdict reason = %q, want the status", got)
	}
}
