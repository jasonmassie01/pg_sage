package cases

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/signalid"
)

func TestProjectIncidentCreatesHighUrgencyCase(t *testing.T) {
	incident := testIncident("prod")
	incident.Severity = SeverityCritical

	got := ProjectIncident(incident)

	if got.SourceType != SourceIncidentType {
		t.Fatalf("SourceType = %q", got.SourceType)
	}
	if got.DatabaseName != "prod" {
		t.Fatalf("DatabaseName = %q", got.DatabaseName)
	}
	if got.WhyNow != "critical incident requires immediate review" {
		t.Fatalf("WhyNow = %q", got.WhyNow)
	}
	if got.Evidence[0].Detail["confidence"] != incident.Confidence {
		t.Fatalf("confidence detail = %#v", got.Evidence[0].Detail)
	}
	if got.Evidence[0].Detail["occurrence_count"] != incident.OccurrenceCount {
		t.Fatalf("occurrence detail = %#v", got.Evidence[0].Detail)
	}
}

// playbooks-01: a PID in prose ("Idle-in-transaction PID 12345") is not a
// backend identity, so the playbook offers the diagnostic and a blocked
// cancel, never a bare-PID cancel or terminate.
func TestIdleTxnIncidentAddsPlaybookCandidates(t *testing.T) {
	incident := testIncident("prod")

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 2 {
		t.Fatalf("ActionCandidates = %d, want 2", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"diagnose_lock_blockers", "safe", "")
	assertCandidate(t, got.ActionCandidates[1], "cancel_backend", "moderate", "")
	assertBlockedSignal(t, got.ActionCandidates[1],
		"no single structured blocker identity")
	assertNoBarePIDSignal(t, got)
}

func TestIdleTxnPlaybookBlocksPidActionsWithoutSinglePID(t *testing.T) {
	incident := testIncident("prod")
	incident.RootCause = "Idle transactions are elevated"
	incident.SignalIDs = []string{signalid.IdleInTxElevated, signalid.ConnectionsHigh}
	incident.CausalChain = nil

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 2 {
		t.Fatalf("ActionCandidates = %d, want 2", len(got.ActionCandidates))
	}
	assertBlockedSignal(t, got.ActionCandidates[1],
		"no single structured blocker identity")
}

func TestNonActionableIncidentHasNoCandidates(t *testing.T) {
	incident := SourceIncident{
		ID:           "inc-2",
		DatabaseName: "prod",
		Severity:     SeverityWarning,
		RootCause:    "WAL archive command failed",
		SignalIDs:    []string{signalid.LogArchiveFailed},
		Source:       "log_deterministic",
		Confidence:   0.8,
	}

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 0 {
		t.Fatalf("expected no action candidates, got %d",
			len(got.ActionCandidates))
	}
}

func TestRunawayQueryIncidentAddsCancelPlaybook(t *testing.T) {
	incident := SourceIncident{
		ID:           "inc-runaway",
		DatabaseName: "prod",
		Severity:     SeverityWarning,
		RootCause:    "Query PID 222 ran for 45 minutes and is spilling to disk",
		SignalIDs:    []string{signalid.LogSlowQuery},
		Source:       "rca",
		Confidence:   0.86,
		CausalChain: []IncidentChainLink{{
			Order:    1,
			Signal:   signalid.LogSlowQuery,
			Evidence: "pid 222 query_age=45m temp_bytes=8GB",
		}},
	}

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 2 {
		t.Fatalf("ActionCandidates = %d, want 2", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"diagnose_runaway_query", "safe", "")
	if !strings.HasPrefix(got.ActionCandidates[0].ProposedSQL,
		"/* pg_sage sre:long_transactions ") ||
		strings.Contains(got.ActionCandidates[0].ProposedSQL, "pid = 222") {
		t.Fatalf("runaway diagnostic must not pin a prose PID: %s",
			got.ActionCandidates[0].ProposedSQL)
	}
	assertCandidate(t, got.ActionCandidates[1], "cancel_backend", "moderate", "")
	assertBlockedSignal(t, got.ActionCandidates[1], "no structured backend identity")
	assertNoBarePIDSignal(t, got)
}

func TestConnectionExhaustionIncidentAddsPoolPlaybook(t *testing.T) {
	incident := SourceIncident{
		ID:           "inc-connections",
		DatabaseName: "prod",
		Severity:     SeverityCritical,
		RootCause:    "Connections are at 96 percent of max_connections",
		SignalIDs:    []string{signalid.ConnectionsHigh},
		Source:       "rca",
		Confidence:   0.9,
	}

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"diagnose_connection_exhaustion", "safe", "")
	if got.ActionCandidates[0].BlockedReason != "" {
		t.Fatalf("diagnostic should not be blocked: %q",
			got.ActionCandidates[0].BlockedReason)
	}
}

func TestWalReplicationIncidentAddsReadOnlyPlaybook(t *testing.T) {
	incident := SourceIncident{
		ID:           "inc-wal",
		DatabaseName: "prod",
		Severity:     SeverityCritical,
		RootCause:    "Replica lag and inactive replication slot are retaining WAL",
		SignalIDs:    []string{signalid.ReplicationLagIncreasing},
		Source:       "rca",
		Confidence:   0.88,
	}

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"diagnose_wal_replication", "safe", "")
	if got.ActionCandidates[0].ProposedSQL == "" {
		t.Fatal("expected read-only WAL/replication diagnostic SQL")
	}
}

func TestAutovacuumFallingBehindIncidentAddsMaintenancePlaybook(t *testing.T) {
	incident := SourceIncident{
		ID:              "inc-av",
		DatabaseName:    "prod",
		Severity:        SeverityWarning,
		RootCause:       "Autovacuum falling behind -- dead tuple accumulation",
		SignalIDs:       []string{signalid.VacuumBlocked},
		AffectedObjects: []string{"public.orders"},
		Source:          "rca",
		Confidence:      0.84,
	}

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 2 {
		t.Fatalf("ActionCandidates = %d, want 2", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"diagnose_vacuum_pressure", "safe", "")
	assertCandidate(t, got.ActionCandidates[1],
		"vacuum_table", "safe", `VACUUM "public"."orders";`)
}

func TestStandbyConflictIncidentAddsReplicaConflictPlaybook(t *testing.T) {
	incident := SourceIncident{
		ID:           "inc-standby",
		DatabaseName: "replica",
		Severity:     SeverityWarning,
		RootCause:    "Standby conflict cancelled queries during replay",
		SignalIDs:    []string{signalid.LogReplicationConflict},
		Source:       "rca",
		Confidence:   0.79,
	}

	got := ProjectIncident(incident)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"diagnose_standby_conflicts", "safe", "")
	if got.ActionCandidates[0].ProposedSQL == "" {
		t.Fatal("expected standby conflict diagnostic SQL")
	}
}

func TestResolvedIncidentProjectsResolvedState(t *testing.T) {
	incident := testIncident("prod")
	resolved := time.Now().UTC()
	incident.ResolvedAt = &resolved

	got := ProjectIncident(incident)

	if got.State != StateResolved {
		t.Fatalf("State = %q, want resolved", got.State)
	}
	if len(got.ActionCandidates) != 0 {
		t.Fatalf("resolved incident should not have candidates")
	}
}

func TestIncidentIdentityIncludesDatabase(t *testing.T) {
	a := ProjectIncident(testIncident("prod"))
	b := ProjectIncident(testIncident("stage"))

	if a.IdentityKey == b.IdentityKey {
		t.Fatalf("identity keys should differ: %q", a.IdentityKey)
	}
}

func testIncident(database string) SourceIncident {
	return SourceIncident{
		ID:              "inc-1",
		DatabaseName:    database,
		Severity:        SeverityWarning,
		RootCause:       "Idle-in-transaction PID 12345 is blocking vacuum",
		SignalIDs:       []string{signalid.VacuumBlocked},
		AffectedObjects: []string{"public.orders"},
		RecommendedSQL:  "SELECT pg_terminate_backend(12345)",
		ActionRisk:      "high_risk",
		Source:          "rca",
		Confidence:      0.91,
		OccurrenceCount: 3,
		DetectedAt:      time.Now().UTC().Add(-time.Hour),
		LastDetectedAt:  time.Now().UTC(),
		CausalChain: []IncidentChainLink{{
			Order:       1,
			Signal:      signalid.VacuumBlocked,
			Description: "vacuum blocked by idle transaction",
			Evidence:    "blocker pid 12345",
		}},
	}
}

func assertCandidate(
	t *testing.T,
	got ActionCandidate,
	actionType string,
	riskTier string,
	sql string,
) {
	t.Helper()
	if got.ActionType != actionType {
		t.Fatalf("ActionType = %q, want %q", got.ActionType, actionType)
	}
	if got.RiskTier != riskTier {
		t.Fatalf("RiskTier = %q, want %q", got.RiskTier, riskTier)
	}
	if sql != "" && got.ProposedSQL != sql {
		t.Fatalf("ProposedSQL = %q, want %q", got.ProposedSQL, sql)
	}
}

// idleTxSaturationIncident is the incident rca/trees.go (treeConnectionsHigh,
// idle_in_tx_elevated branch) emits, verbatim. Its recommended SQL ends in
// "LIMIT 20", which the old prose regex read as PID 20.
func idleTxSaturationIncident(severity Severity) SourceIncident {
	return SourceIncident{
		ID:           "inc-idle-sat",
		DatabaseName: "prod",
		Severity:     severity,
		RootCause:    "Idle-in-transaction sessions saturating connection pool",
		CausalChain: []IncidentChainLink{
			{Order: 1, Signal: signalid.IdleInTxElevated,
				Description: "Sessions stuck in idle-in-transaction state",
				Evidence:    "37 idle-in-tx sessions"},
			{Order: 2, Signal: signalid.ConnectionsHigh,
				Description: "Connection slots exhausted by idle sessions",
				Evidence:    "96/100 connections used"},
		},
		AffectedObjects: []string{"pg_stat_activity"},
		SignalIDs:       []string{signalid.ConnectionsHigh, signalid.IdleInTxElevated},
		RecommendedSQL: "SELECT pid, state, query, now()-state_change AS duration " +
			"FROM pg_stat_activity " +
			"WHERE state = 'idle in transaction' " +
			"ORDER BY state_change LIMIT 20;",
		ActionRisk: "moderate",
		Source:     "deterministic",
		Confidence: 0.85,
	}
}

// Invalid input: the verbatim idle-tx saturation incident has no backend
// identity, so it yields the diagnostic plus a blocked cancel and no signal
// SQL at any severity (it produced pg_cancel_backend(20) and HIGH
// pg_terminate_backend(20) before playbooks-01).
func TestIdleTxSaturationIncidentYieldsNoSignalSQL(t *testing.T) {
	for _, severity := range []Severity{SeverityWarning, SeverityCritical} {
		got := ProjectIncident(idleTxSaturationIncident(severity))

		if len(got.ActionCandidates) != 2 {
			t.Fatalf("%s: ActionCandidates = %+v, want diagnostic + blocked cancel",
				severity, got.ActionCandidates)
		}
		assertCandidate(t, got.ActionCandidates[0],
			"diagnose_lock_blockers", "safe", "")
		if got.ActionCandidates[0].ProposedSQL != diagnosticSQL("diagnose_lock_blockers") {
			t.Fatalf("%s: diagnostic SQL changed", severity)
		}
		assertBlockedSignal(t, got.ActionCandidates[1],
			"no single structured blocker identity")
		assertNoBarePIDSignal(t, got)
	}
}

// Invalid input: "pid unknown for 30 minutes" is prose, not PID 30.
func TestProsePIDYieldsNoSignalSQL(t *testing.T) {
	runaway := SourceIncident{
		ID: "inc-prose", DatabaseName: "prod", Severity: SeverityCritical,
		RootCause: "Runaway query: pid unknown for 30 minutes",
		SignalIDs: []string{signalid.LogSlowQuery}, Source: "rca", Confidence: 0.8,
	}
	lock := idleTxSaturationIncident(SeverityCritical)
	lock.RootCause = "Blocker pid unknown for 30 minutes on a connection"

	for _, incident := range []SourceIncident{runaway, lock} {
		got := ProjectIncident(incident)

		assertNoBarePIDSignal(t, got)
		for _, c := range got.ActionCandidates {
			if strings.Contains(c.ProposedSQL, "30") {
				t.Fatalf("%s: %s SQL uses the prose number: %s",
					incident.ID, c.ActionType, c.ProposedSQL)
			}
			if c.ActionType == "terminate_backend" {
				t.Fatalf("%s: prose incident offered terminate", incident.ID)
			}
		}
	}
}

var barePIDSignal = regexp.MustCompile(`pg_(cancel|terminate)_backend\(\d+\)`)

// assertNoBarePIDSignal is CHECK-02: no candidate proposes a signal for a
// literal PID.
func assertNoBarePIDSignal(t *testing.T, c Case) {
	t.Helper()
	for _, candidate := range c.ActionCandidates {
		if barePIDSignal.MatchString(candidate.ProposedSQL) {
			t.Fatalf("%s proposes a bare-PID signal: %q",
				candidate.ActionType, candidate.ProposedSQL)
		}
	}
}

func assertBlockedSignal(t *testing.T, c ActionCandidate, reason string) {
	t.Helper()
	if !strings.Contains(c.BlockedReason, reason) {
		t.Fatalf("%s BlockedReason = %q, want it to contain %q",
			c.ActionType, c.BlockedReason, reason)
	}
	if c.ProposedSQL != "" {
		t.Fatalf("blocked %s still proposes SQL: %q", c.ActionType, c.ProposedSQL)
	}
	if c.ExpiresAt == nil {
		t.Fatalf("blocked %s has no expiry", c.ActionType)
	}
}
