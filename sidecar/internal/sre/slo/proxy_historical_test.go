package slo

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

type historicalSourceFunc func(context.Context, probes.ID, probes.Args) probes.Result

func (f historicalSourceFunc) ReadHistory(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	return f(ctx, id, a)
}

func TestHistoricalErrorProxyNeverTurnsSamplesIntoHealthySLO(t *testing.T) {
	for _, rows := range [][]probes.Row{nil, {{"sqlstate": "40P01", "xact_commit": 100.0}}} {
		calls := 0
		p := NewHistoricalErrorProxy(historicalSourceFunc(func(_ context.Context, id probes.ID, a probes.Args) probes.Result {
			calls++
			return probes.Result{ProbeID: id, Status: probes.StatusOK, History: &probes.HistoricalWindow{}, Rows: rows}
		}), DefaultProxyConfig())
		got := p.Slice(context.Background(), time.Now(), nil)
		if calls != 2 || got.Eligible != 0 || got.Bad != 0 || got.Reason != ReasonHistoricalIncomplete {
			t.Fatalf("historical samples manufactured health: %+v calls=%d", got, calls)
		}
	}
}

func TestHistoricalErrorProxyPreservesPolicyAndAvailabilityReasons(t *testing.T) {
	for _, tc := range []struct {
		result probes.Result
		reason string
	}{
		{probes.Result{Status: probes.StatusUnsupported, Reason: probes.ReasonPolicyDenied}, ReasonSourcePolicyDenied},
		{probes.Result{Status: probes.StatusUnsupported, Reason: "history_policy_revoked"}, ReasonSourcePolicyDenied},
		{probes.Result{Status: probes.StatusError, Reason: "history_identity_unavailable"}, ReasonSourceError},
		{probes.Result{Status: probes.StatusUnsupported, Reason: "history_database_identity_mismatch"}, ReasonSourceUnavailable},
		{probes.Result{Status: probes.StatusUnsupported, Reason: "historical_no_observations_completeness_unknown"}, ReasonHistoricalIncomplete},
	} {
		p := NewHistoricalErrorProxy(historicalSourceFunc(func(context.Context, probes.ID, probes.Args) probes.Result { return tc.result }), DefaultProxyConfig())
		got := p.Slice(context.Background(), time.Now(), nil)
		if got.Reason != tc.reason || got.Bad != 0 || got.Eligible != 0 {
			t.Fatalf("%s: %+v", tc.reason, got)
		}
	}
}
