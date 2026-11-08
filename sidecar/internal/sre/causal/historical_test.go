package causal

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func historicalObservation(id probes.ID, source string) Observation {
	at := time.Now().UTC().Add(-time.Hour)
	return Observation{EvidenceID: "citable-history", Result: probes.Result{ProbeID: id, Status: probes.StatusOK,
		History: &probes.HistoricalWindow{ScopeHash: "trusted-scope", From: at.Add(-time.Minute), To: at.Add(time.Minute)},
		Rows: []probes.Row{{"event_id": "provider-event", "source": source, "scope_hash": "trusted-scope", "occurred_at": at,
			"historical_only": true, "complete": false, "exact_multiplicity": false, "rca_signal": "log_deadlock_detected", "command": "ALTER TABLE", "object_type": "TABLE"}}}}
}

func TestHistoricalContextRetainsCitationsWithoutChangingLiveConclusions(t *testing.T) {
	root := &Hypothesis{Node: IdleInTxHolder, Confidence: .9, Status: StatusRoot}
	d := Diagnosis{Root: root, Conclusive: true, Impact: &Impact{State: ImpactUnknown}}
	obs := []Observation{historicalObservation(probes.LakebaseLogs, "postgres_logs"), historicalObservation(probes.LakebaseChanges, "ddl_history")}
	got := WithHistorical(d, obs)
	if !reflect.DeepEqual(got.Root, root) || !got.Conclusive || !reflect.DeepEqual(got.Impact, d.Impact) || len(got.Alternatives) != 0 || len(got.RuledOut) != 0 {
		t.Fatalf("historical context changed live conclusion: %+v", got)
	}
	if len(got.Observed) != 2 || got.Observed[0].EvidenceID != "citable-history" || !strings.Contains(got.Observed[0].Text, "log_deadlock_detected") ||
		!strings.Contains(got.Observed[1].Text, "incident correlation and causal mechanism are unproven") || !strings.Contains(got.Observed[1].Text, "provider-event") {
		t.Fatalf("structured historical context lost: %+v", got.Observed)
	}
}

func TestHistoricalAbsenceAndInvalidProvenanceDoNotRuleOutCauses(t *testing.T) {
	for _, mutate := range []func(*Observation){
		func(o *Observation) { o.Result.Rows = nil },
		func(o *Observation) { o.Result.History.Complete = true },
		func(o *Observation) { o.Result.Rows[0]["scope_hash"] = "other-database" },
		func(o *Observation) { o.Result.Rows[0]["occurred_at"] = time.Now() },
		func(o *Observation) { o.Result.Status = probes.StatusUnsupported },
	} {
		o := historicalObservation(probes.LakebaseChanges, "ddl_history")
		mutate(&o)
		d := WithHistorical(Diagnosis{}, []Observation{o})
		if len(d.Missing) != 1 || len(d.RuledOut) != 0 || d.Root != nil || len(d.Observed) != 0 {
			t.Fatalf("unavailable partial history claimed knowledge: %+v", d)
		}
	}
}
