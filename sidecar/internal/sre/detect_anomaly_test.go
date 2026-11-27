package sre

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// market-02: a critical workload_anomaly finding starts one Sage SRE
// investigation per episode (TriggerAnomaly), keyed
// anomaly:<metric>:<key>:<episode_start>. It triages the lock, connection
// and plan mechanisms read-only, like an operator start.
// No concurrent access tests: anomalyTrigger is a pure decode; the
// store's create race is covered by TestCoordinator_TwoCoordinatorsRace*.

const anomalyDetail = `{"metric": "query_mean_ms", "key": "queryid:101",
	"episode_start": "2026-11-10T09:00:00Z", "queryid": 101, "z": 9.5}`

func TestTriggerAnomaly_IsAKnownKindWithTheTriagePlan(t *testing.T) {
	if TriggerAnomaly != "workload_anomaly" || !triggerKinds[TriggerAnomaly] {
		t.Fatalf("TriggerAnomaly = %q known=%v", TriggerAnomaly, triggerKinds[TriggerAnomaly])
	}
	steps, ok := planFor(TriggerAnomaly, time.Hour)
	if !ok || len(steps) == 0 {
		t.Fatal("no probe plan for workload_anomaly")
	}
	ran := map[probes.ID]bool{}
	for _, s := range steps {
		for _, c := range s.calls {
			ran[c.id] = true
		}
	}
	for _, id := range []probes.ID{probes.LockGraph, probes.ConnectionSaturation,
		probes.PlanRegressions, probes.SageActions} {
		if !ran[id] {
			t.Errorf("plan lacks %s: %v", id, ran)
		}
	}
	d := diagnose(Investigation{TriggerKind: TriggerAnomaly,
		Subject: "query_mean_ms queryid:101"}, nil)
	if d.Family != causal.FamilyAnomaly || d.Conclusive ||
		!strings.Contains(d.Reason, "query_mean_ms queryid:101") {
		t.Fatalf("diagnosis = %+v, want an inconclusive workload_anomaly triage", d)
	}
}

func TestAnomalyTrigger_KeysTheEpisode(t *testing.T) {
	tr, ok := anomalyTrigger("workload", "query_mean_ms:queryid:101", anomalyDetail,
		"orders")
	if !ok {
		t.Fatal("valid detail gave no trigger")
	}
	if tr.Kind != TriggerAnomaly ||
		tr.IdempotencyKey != "anomaly:query_mean_ms:queryid:101:2026-11-10T09:00:00Z" ||
		tr.Subject != "query_mean_ms queryid:101" || tr.CaseID == "" {
		t.Fatalf("trigger = %+v", tr)
	}
	// Invalid input: no episode, no metric, malformed JSON, empty.
	for _, bad := range []string{`{"metric": "m", "key": "k"}`,
		`{"key": "k", "episode_start": "2026-11-10T09:00:00Z"}`, `{not json`, ""} {
		if _, ok := anomalyTrigger("workload", "x", bad, "orders"); ok {
			t.Errorf("detail %q gave a trigger", bad)
		}
	}
	// Boundary: an oversized key is clipped to the request's 160 runes.
	long := `{"metric": "m", "key": "` + strings.Repeat("k", 300) +
		`", "episode_start": "2026-11-10T09:00:00Z"}`
	tr, ok = anomalyTrigger("workload", "x", long, "orders")
	if !ok || utf8.RuneCountInString(tr.IdempotencyKey) > 160 ||
		utf8.RuneCountInString(tr.Subject) > 256 {
		t.Fatalf("long key = %d runes (%v), want at most 160",
			utf8.RuneCountInString(tr.IdempotencyKey), ok)
	}
}
