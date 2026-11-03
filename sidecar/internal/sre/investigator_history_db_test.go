package sre

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

type historicalReaderFunc func(context.Context, probes.ID, probes.Args) probes.Result

func (f historicalReaderFunc) ReadHistory(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	return f(ctx, id, a)
}

func TestInvestigatorHistoricalReadIsDurableCitedAndSeparateFromLiveDiagnosis(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	reader := historicalReaderFunc(func(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
		return probes.Result{ProbeID: id, Version: "v1_history", ObservedAt: time.Now(), Status: probes.StatusOK,
			History: &probes.HistoricalWindow{From: at.Add(-time.Hour), To: at.Add(time.Hour), ScopeHash: strings.Repeat("a", 64)},
			Rows: []probes.Row{{"event_id": "historical-event", "source": "postgres_logs", "scope_hash": strings.Repeat("a", 64), "occurred_at": at,
				"rca_signal": "log_deadlock_detected", "historical_only": true, "complete": false, "exact_multiplicity": false}}}
	})
	m := newFakeModel(t, call(ToolHistory, `{"source":"lakebase_log_history","window_seconds":7200}`), submit(func(body string) invFinal {
		return invFinal{Outcome: "agree", Root: "idle_in_tx_holder", Claims: []invClaim{idleClaim(aliasOf(t, body, probes.LockGraph, "ok")),
			{Text: "Historical deadlock evidence is partial; it does not prove a live incident or complete coverage.", EvidenceIDs: []string{lastAliasOf(t, body, probes.LakebaseLogs, "ok")}}}}
	}))
	c, _ := investigatorCoordinator(t, ctx, st, longTxRunner(), m.client(), invOptions{config: InvestigatorConfig{History: reader}})
	inv := startAndRun(t, ctx, c, lockTrigger("historical"))
	assertIdleRoot(t, st, inv)
	var historicalFact bool
	for _, f := range inv.Summary.Observed {
		if strings.Contains(f.Text, "Historical partial observation") && strings.Contains(f.Text, "historical-event") && strings.Contains(f.Text, "log_deadlock_detected") {
			historicalFact = true
		}
	}
	if !historicalFact {
		t.Fatalf("historical context was not consumed by deterministic summary: %+v", inv.Summary.Observed)
	}
	run := transcriptOf(t, inv)
	steps := stepsOf(run, ToolHistory)
	if len(steps) != 1 || steps[0].EvidenceID == "" {
		t.Fatalf("historical read missing from transcript: %+v", run)
	}
	stored := evidenceByID(t, st, inv)
	e := stored[steps[0].EvidenceID]
	if !e.VerifyHash() || steps[0].Digest != digestOf(e) {
		t.Fatal("historical read did not persist its citable hash")
	}
	var res probes.Result
	if err := json.Unmarshal(e.Payload, &res); err != nil || res.History == nil || res.History.Complete || len(res.Rows) != 1 {
		t.Fatalf("historical provenance lost: %+v %v", res, err)
	}
	assertCitesStored(t, inv, stored, e.ID)
	if err := st.VerifyEvents(ctx, inv.Scope, inv.ID); err != nil {
		t.Fatal(err)
	}
}
