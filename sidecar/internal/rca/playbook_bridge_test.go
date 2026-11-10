package rca

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/signalid"
)

// playbooks-02 bridge: incidents built by the real decision trees are
// projected into cases, so the playbook table is pinned against what RCA
// actually writes instead of ids invented in cases tests.

// projectCase maps an RCA incident to the case projection input the way
// the API reads a sage.incidents row: the causal chain through JSON.
func projectCase(t *testing.T, inc Incident) cases.Case {
	t.Helper()
	raw, err := json.Marshal(inc.CausalChain)
	if err != nil {
		t.Fatalf("marshal chain: %v", err)
	}
	var chain []cases.IncidentChainLink
	if err := json.Unmarshal(raw, &chain); err != nil {
		t.Fatalf("unmarshal chain into cases links: %v", err)
	}
	return cases.ProjectIncident(cases.SourceIncident{
		ID: "inc-bridge", DatabaseName: "prod", Severity: cases.Severity(inc.Severity),
		RootCause: inc.RootCause, CausalChain: chain, AffectedObjects: inc.AffectedObjects,
		SignalIDs: inc.SignalIDs, RecommendedSQL: inc.RecommendedSQL,
		ActionRisk: inc.ActionRisk, Source: inc.Source, Confidence: inc.Confidence,
		DetectedAt: inc.DetectedAt, LastDetectedAt: inc.DetectedAt,
	})
}

func caseActionTypes(c cases.Case) []string {
	out := []string{}
	for _, candidate := range c.ActionCandidates {
		out = append(out, candidate.ActionType)
	}
	return out
}

func firedSignal(id string, metrics map[string]any) *Signal {
	return &Signal{ID: id, FiredAt: time.Now(), Severity: "warning", Metrics: metrics}
}

// snapshotIncident runs the snapshot decision trees on one fired signal.
func snapshotIncident(t *testing.T, sig *Signal) Incident {
	t.Helper()
	curr := &collector.Snapshot{CollectedAt: time.Now(),
		System: collector.SystemStats{TotalBackends: 90, MaxConnections: 100}}
	got := newTestEngine().runDecisionTrees([]*Signal{sig}, curr, nil, defaultCfg())
	if len(got) != 1 {
		t.Fatalf("%s: decision trees built %d incidents, want 1", sig.ID, len(got))
	}
	return got[0]
}

func logIncident(t *testing.T, sig *Signal) Incident {
	t.Helper()
	got := newTestEngine().runLogDecisionTrees([]*Signal{sig})
	if len(got) != 1 {
		t.Fatalf("%s: log trees built %d incidents, want 1", sig.ID, len(got))
	}
	return got[0]
}

// Happy path (fails before playbooks-02): replication lag and WAL growth
// incidents reach the WAL playbook.
func TestRealWALIncidentsGetWALDiagnostic(t *testing.T) {
	lag := "00:01:00"
	addr := "10.0.0.2"
	snap := &collector.Snapshot{CollectedAt: time.Now(),
		Replication: &collector.ReplicationStats{Replicas: []collector.ReplicaInfo{
			{ClientAddr: &addr, ReplayLag: &lag}}}}
	lagSig := newTestEngine().detectReplicationLag(snap, nil, defaultCfg())
	if lagSig == nil {
		t.Fatal("fixture did not fire replication_lag_increasing")
	}
	wal := firedSignal(signalid.WALGrowthSpike, map[string]any{"ratio": 3.0,
		"current_wal_bytes": int64(3 << 30), "previous_wal_bytes": int64(1 << 30)})
	for _, sig := range []*Signal{lagSig, wal} {
		got := caseActionTypes(projectCase(t, snapshotIncident(t, sig)))
		if !reflect.DeepEqual(got, []string{"diagnose_wal_replication"}) {
			t.Errorf("%s: candidates = %v, want [diagnose_wal_replication]", sig.ID, got)
		}
	}
}

// Boundary (fails before playbooks-02): the OOM tree's text names
// max_connections, which used to route it to the connection diagnostic.
func TestRealOOMIncidentGetsNoConnectionDiagnostic(t *testing.T) {
	inc := logIncident(t, firedSignal(signalid.LogOutOfMemory,
		map[string]any{"database": "appdb"}))
	if !strings.Contains(inc.RecommendedSQL+inc.CausalChain[1].Description,
		"max_connections") {
		t.Fatal("fixture no longer exercises the max_connections wording")
	}
	if got := caseActionTypes(projectCase(t, inc)); len(got) != 0 {
		t.Fatalf("OOM incident candidates = %v, want none", got)
	}
}

func TestRealLogIncidentsRouteThroughTable(t *testing.T) {
	for _, test := range []struct {
		id   string
		want []string
	}{
		{signalid.LogReplicationConflict, []string{"diagnose_standby_conflicts"}},
		{signalid.LogReplicationSlotInactive, []string{"diagnose_wal_replication"}},
		{signalid.LogWALSegmentRemoved, []string{"diagnose_wal_replication"}},
		{signalid.LogSlowQuery, []string{"diagnose_runaway_query", "cancel_backend"}},
		{signalid.LogStatementTimeout, []string{"diagnose_runaway_query", "cancel_backend"}},
		{signalid.LogTxidWraparoundWarning, []string{"diagnose_freeze_blockers"}},
		{signalid.LogAutovacuumCancel, []string{"diagnose_vacuum_pressure"}},
		{signalid.LogConnectionRefused, []string{"diagnose_connection_exhaustion"}},
		{signalid.LogDiskFull, []string{}},
		{signalid.LogArchiveFailed, []string{}},
	} {
		sig := firedSignal(test.id, map[string]any{"database": "appdb",
			"message": "duration: 90000.123 ms statement: SELECT 1"})
		got := caseActionTypes(projectCase(t, logIncident(t, sig)))
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: candidates = %v, want %v", test.id, got, test.want)
		}
	}
}

func TestRealVacuumBlockedFallingBehindGetsVacuumPressure(t *testing.T) {
	sig := firedSignal(signalid.VacuumBlocked,
		map[string]any{"blocked_tables": []string{"public.orders"}})
	c := projectCase(t, snapshotIncident(t, sig))
	got := caseActionTypes(c)
	if !reflect.DeepEqual(got, []string{"diagnose_vacuum_pressure", "vacuum_table"}) {
		t.Fatalf("candidates = %v", got)
	}
	if sql := c.ActionCandidates[1].ProposedSQL; sql != `VACUUM "public"."orders";` {
		t.Fatalf("vacuum SQL = %q", sql)
	}
}

func TestRealSnapshotIncidentsRouteThroughTable(t *testing.T) {
	for _, test := range []struct {
		sig  *Signal
		want []string
	}{
		{firedSignal(signalid.ConnectionsHigh, map[string]any{"pct": 90}),
			[]string{"diagnose_connection_exhaustion"}},
		{firedSignal(signalid.CacheHitRatioDrop, map[string]any{"cache_hit_ratio": 0.8}),
			[]string{}},
		{firedSignal(signalid.OrphanedPreparedTx, map[string]any{"count": 1,
			"oldest_gid": "g1", "max_xid_age": int64(5000)}),
			[]string{"diagnose_freeze_blockers"}},
	} {
		got := caseActionTypes(projectCase(t, snapshotIncident(t, test.sig)))
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: candidates = %v, want %v", test.sig.ID, got, test.want)
		}
	}
}

// Contract: every log id signalid lists becomes exactly one incident that
// carries that id, so the cases table sees what the log trees write.
func TestEveryLogSignalBuildsAnIncidentWithItsID(t *testing.T) {
	for _, info := range signalid.All() {
		if info.Family != signalid.FamilyLog {
			continue
		}
		inc := logIncident(t, firedSignal(info.ID, map[string]any{"database": "appdb"}))
		if !reflect.DeepEqual(inc.SignalIDs, []string{info.ID}) {
			t.Errorf("%s: incident signal ids = %v", info.ID, inc.SignalIDs)
		}
	}
}

// Invalid input: a signal id outside the vocabulary builds no incident.
func TestUnknownSignalBuildsNoIncident(t *testing.T) {
	sig := firedSignal("replica_lag", nil)
	curr := &collector.Snapshot{CollectedAt: time.Now()}
	if got := newTestEngine().runDecisionTrees([]*Signal{sig}, curr, nil,
		defaultCfg()); len(got) != 0 {
		t.Fatalf("snapshot trees built %d incidents for an unknown id", len(got))
	}
	if got := newTestEngine().runLogDecisionTrees([]*Signal{sig}); len(got) != 0 {
		t.Fatalf("log trees built %d incidents for an unknown id", len(got))
	}
}

// A vacuum_blocked incident from the prepared-transaction branch carries a
// structured holder, so the case offers the read-only freeze-blocker
// diagnostic and no VACUUM the holder would defeat.
func TestRealVacuumBlockedPreparedHolderGetsFreezeBlockers(t *testing.T) {
	now := time.Now()
	curr := &collector.Snapshot{CollectedAt: now,
		System: collector.SystemStats{TotalBackends: 5, MaxConnections: 100},
		PreparedXacts: []collector.PreparedTransaction{{GID: "g-old",
			Prepared: now.Add(-48 * time.Hour), Database: "prod", XIDAge: 90000}}}
	sig := firedSignal(signalid.VacuumBlocked,
		map[string]any{"blocked_tables": []string{"public.orders"}})
	incs := newTestEngine().runDecisionTrees([]*Signal{sig}, curr, nil, defaultCfg())
	if len(incs) != 1 || incs[0].CausalChain[0].Blocker == nil {
		t.Fatalf("fixture did not build a prepared-holder incident: %+v", incs)
	}
	got := caseActionTypes(projectCase(t, incs[0]))
	if !reflect.DeepEqual(got, []string{"diagnose_freeze_blockers"}) {
		t.Fatalf("candidates = %v, want [diagnose_freeze_blockers]", got)
	}
}
