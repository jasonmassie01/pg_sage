package executor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/verify"
)

// executor-03b: the outcome text of an index verdict names the table-scoped
// write cost when it rose over write_impact_pct: the cause of a write_impact
// revert, or evidence beside a query reason that came first.
// No concurrent-access tests: outcomeReason is a pure function of a verdict.

func signalVerdict(signal map[string]any) verify.Verdict {
	return verify.Verdict{Revert: true, Status: "reverted", Reason: "write_impact",
		Evidence: map[string]any{
			"comparison":   map[string]any{"reason": "gain_confirmed"},
			"write_signal": signal,
		}}
}

func TestOutcomeReasonNamesTableWriteRegression(t *testing.T) {
	got := outcomeReason(signalVerdict(map[string]any{
		"scope": "table", "metric": "wal_bytes_per_tuple", "table": "public.orders",
		"delta_pct": 50.0, "threshold_pct": 20.0, "exceeds_threshold": true,
		"decisive": true,
	}))
	want := "write_impact: gain_confirmed; WAL bytes per tuple written to public.orders " +
		"+50.0% (over the 20% write_impact_pct)"
	if got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	for _, stale := range []string{"database-wide", "advisory"} {
		if strings.Contains(got, stale) {
			t.Fatalf("reason = %q still says %q", got, stale)
		}
	}
}

func TestOutcomeReasonOmitsSignalUnderThreshold(t *testing.T) {
	cases := map[string]map[string]any{
		"under threshold": {"delta_pct": 10.0, "threshold_pct": 20.0,
			"exceeds_threshold": false},
		"read error":   {"error": "read post-action write cost: boom"},
		"insufficient": {"insufficient": "no attributable table writes"},
		"wrong type":   {"exceeds_threshold": "true"},
		"empty signal": {},
	}
	for name, signal := range cases {
		v := signalVerdict(signal)
		v.Reason = ""
		if got := outcomeReason(v); got != "gain_confirmed" {
			t.Fatalf("%s: reason = %q, want just the query reason", name, got)
		}
	}
}

func TestOutcomeReasonKeepsRevertReasonAndStatusFallback(t *testing.T) {
	revert := verify.Verdict{Revert: true, Status: "reverted", Reason: "no_gain",
		Evidence: map[string]any{"comparison": map[string]any{"reason": "below_gain"},
			"write_signal": map[string]any{"delta_pct": 80.0, "threshold_pct": 20.0,
				"exceeds_threshold": true, "table": "public.t"}}}
	got := outcomeReason(revert)
	if !strings.HasPrefix(got, "no_gain: below_gain; ") ||
		!strings.Contains(got, "public.t +80.0%") {
		t.Fatalf("revert reason = %q, want no_gain: below_gain then the write cost", got)
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
