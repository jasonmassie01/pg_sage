package cases

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/signalid"
)

// rcaShapedIncident is an incident as RCA writes it: one signal id, the
// id on every chain link, source "deterministic" or "log_deterministic".
func rcaShapedIncident(signal, rootCause string, affected ...string) SourceIncident {
	return SourceIncident{
		ID: "inc-" + signal, DatabaseName: "prod", Severity: SeverityWarning,
		RootCause: rootCause, SignalIDs: []string{signal},
		CausalChain: []IncidentChainLink{{Order: 1, Signal: signal,
			Description: rootCause, Evidence: rootCause}},
		AffectedObjects: affected, ActionRisk: "safe", Source: "deterministic",
		Confidence: 0.85, LastDetectedAt: evidenceAnchor,
	}
}

func actionTypes(c Case) []string {
	out := make([]string, 0, len(c.ActionCandidates))
	for _, candidate := range c.ActionCandidates {
		out = append(out, candidate.ActionType)
	}
	return out
}

func assertActionTypes(t *testing.T, label string, c Case, want ...string) {
	t.Helper()
	if got := actionTypes(c); !reflect.DeepEqual(got, want) &&
		!(len(got) == 0 && len(want) == 0) {
		t.Fatalf("%s: action types = %v, want %v", label, got, want)
	}
}

func TestWALSignalsOfferReadOnlyWALDiagnostic(t *testing.T) {
	for _, signal := range []string{
		signalid.ReplicationLagIncreasing, signalid.WALGrowthSpike,
		signalid.LogReplicationSlotInactive, signalid.LogWALSegmentRemoved,
	} {
		got := ProjectIncident(rcaShapedIncident(signal, "WAL is retained"))

		assertActionTypes(t, signal, got, "diagnose_wal_replication")
		c := got.ActionCandidates[0]
		assertCandidate(t, c, "diagnose_wal_replication", "safe",
			diagnosticSQL("diagnose_wal_replication"))
		if c.BlockedReason != "" || c.RollbackClass != "not_applicable" {
			t.Fatalf("%s: diagnostic blocked=%q rollback=%q", signal, c.BlockedReason,
				c.RollbackClass)
		}
		if strings.Contains(c.ProposedSQL, "pg_drop_replication_slot") {
			t.Fatalf("%s: WAL diagnostic drops a slot", signal)
		}
	}
}

func TestStandbyConflictLogOffersStandbyDiagnostic(t *testing.T) {
	got := ProjectIncident(rcaShapedIncident(signalid.LogReplicationConflict,
		"Replication conflict with recovery on standby"))

	assertActionTypes(t, "standby", got, "diagnose_standby_conflicts")
	assertCandidate(t, got.ActionCandidates[0], "diagnose_standby_conflicts", "safe",
		diagnosticSQL("diagnose_standby_conflicts"))
}

func TestRunawayLogSignalsOfferRunawayPlaybook(t *testing.T) {
	for _, signal := range []string{signalid.LogSlowQuery, signalid.LogStatementTimeout} {
		got := ProjectIncident(rcaShapedIncident(signal, "Statement timeout exceeded"))

		assertActionTypes(t, signal, got, "diagnose_runaway_query", "cancel_backend")
		assertCandidate(t, got.ActionCandidates[0], "diagnose_runaway_query", "safe",
			diagnosticSQL("diagnose_runaway_query"))
		assertBlockedSignal(t, got.ActionCandidates[1], noBackendIdentityReason)
		assertNoBarePIDSignal(t, got)
	}
}

func TestConnectionSignalsOfferConnectionDiagnostic(t *testing.T) {
	for _, signal := range []string{signalid.ConnectionsHigh, signalid.LogConnectionRefused} {
		got := ProjectIncident(rcaShapedIncident(signal, "too many clients already"))

		assertActionTypes(t, signal, got, "diagnose_connection_exhaustion")
		assertCandidate(t, got.ActionCandidates[0], "diagnose_connection_exhaustion",
			"safe", diagnosticSQL("diagnose_connection_exhaustion"))
	}
}

// Boundary: the OOM tree's text names max_connections; that word alone no
// longer routes the incident to the connection diagnostic.
func TestOOMIncidentOffersNoConnectionDiagnostic(t *testing.T) {
	oom := rcaShapedIncident(signalid.LogOutOfMemory,
		"Out of memory -- work_mem × active sessions exceeds available RAM", "prod")
	oom.RecommendedSQL = "SELECT name, setting, unit FROM pg_settings WHERE name IN " +
		"('work_mem','shared_buffers','max_connections','maintenance_work_mem');"
	oom.CausalChain = append(oom.CausalChain, IncidentChainLink{Order: 2,
		Signal:      signalid.LogOutOfMemory,
		Description: "Reduce work_mem or max_connections to fit within available RAM"})

	got := ProjectIncident(oom)

	assertActionTypes(t, "oom", got)
}

func TestVacuumBlockedFallingBehindOffersVacuumPressure(t *testing.T) {
	got := ProjectIncident(rcaShapedIncident(signalid.VacuumBlocked,
		"Autovacuum falling behind -- dead tuple accumulation", "public.orders"))

	assertActionTypes(t, "falling behind", got, "diagnose_vacuum_pressure", "vacuum_table")
	assertCandidate(t, got.ActionCandidates[0], "diagnose_vacuum_pressure", "safe",
		diagnosticSQL("diagnose_vacuum_pressure"))
	assertCandidate(t, got.ActionCandidates[1], "vacuum_table", "safe",
		`VACUUM "public"."orders";`)
}

// A vacuum_blocked holder branch (structured holder on the chain) that is
// not an idle-in-transaction session gets the read-only freeze-blocker
// diagnostic, never a VACUUM that the holder would defeat.
func TestVacuumBlockedHolderOffersFreezeBlockers(t *testing.T) {
	holder := rcaShapedIncident(signalid.VacuumBlocked,
		`Replication slot "s1" holding oldest xmin (age 900), blocking autovacuum`,
		"public.orders")
	holder.CausalChain[0].Blocker = &IncidentBlocker{State: "inactive"}

	got := ProjectIncident(holder)

	assertActionTypes(t, "holder", got, "diagnose_freeze_blockers")
	assertCandidate(t, got.ActionCandidates[0], "diagnose_freeze_blockers", "safe",
		diagnosticSQL("diagnose_freeze_blockers"))
}

// The log tree's affected object is the database, not a table: the
// cancel incident gets the unscoped diagnostic and no VACUUM.
func TestAutovacuumCancelOffersDiagnosticOnly(t *testing.T) {
	got := ProjectIncident(rcaShapedIncident(signalid.LogAutovacuumCancel,
		"Autovacuum task cancelled due to lock conflict", "appdb"))

	assertActionTypes(t, "autovacuum cancel", got, "diagnose_vacuum_pressure")
	sql := got.ActionCandidates[0].ProposedSQL
	if sql != diagnosticSQL("diagnose_vacuum_pressure") {
		t.Fatalf("cancel diagnostic is scoped to the database name: %s", sql)
	}
}

func TestFreezeSignalsOfferFreezeDiagnostic(t *testing.T) {
	for _, signal := range []string{signalid.LogTxidWraparoundWarning,
		signalid.OrphanedPreparedTx} {
		got := ProjectIncident(rcaShapedIncident(signal, "xmin horizon held back"))

		assertActionTypes(t, signal, got, "diagnose_freeze_blockers")
		c := got.ActionCandidates[0]
		assertCandidate(t, c, "diagnose_freeze_blockers", "safe",
			diagnosticSQL("diagnose_freeze_blockers"))
		if !reflect.DeepEqual(c.VerificationPlan,
			verificationPlanForAction("diagnose_freeze_blockers")) {
			t.Fatalf("%s: verification plan = %v", signal, c.VerificationPlan)
		}
	}
}

// Every id decided "none" projects a case with no candidates.
func TestNoneSignalsOfferNothing(t *testing.T) {
	for id, playbook := range wantSignalPlaybooks {
		if playbook != playbookNone {
			continue
		}
		got := ProjectIncident(rcaShapedIncident(id,
			"runaway query ran for 45m near max_connections; sequence exhausted", "prod"))
		assertActionTypes(t, id, got)
	}
}

// Invalid input: an unknown id gives no candidate, whatever its prose says.
func TestUnknownSignalOffersNothing(t *testing.T) {
	for _, id := range []string{"replication_lag", "inactive_slot", "runaway_query",
		"sequence_exhaustion", "autovacuum_falling_behind", "standby_conflict"} {
		got := ProjectIncident(rcaShapedIncident(id,
			"Runaway query ran for 45 minutes; sequence exhausted at max_connections"))
		assertActionTypes(t, id, got)
	}
}

// Rewording a root cause, chain or SQL does not change the candidates. The
// lock and idle-tx playbook (playbooks-01) still reads its blocker prose,
// so lock_contention, idle_in_tx_elevated and vacuum_blocked are excluded.
func TestRewordingRootCauseKeepsCandidates(t *testing.T) {
	reword := "Runaway query ran for 45 minutes; sequence exhaustion near " +
		"max_connections; connection exhaustion"
	for id, playbook := range wantSignalPlaybooks {
		if playbook == playbookLock || id == signalid.VacuumBlocked {
			continue
		}
		plain := rcaShapedIncident(id, "plain", "public.t")
		noisy := plain
		noisy.RootCause, noisy.RecommendedSQL = reword, reword
		noisy.CausalChain = []IncidentChainLink{{Order: 1, Signal: id,
			Description: reword, Evidence: reword}}
		if a, b := ProjectIncident(plain), ProjectIncident(noisy); !reflect.DeepEqual(
			actionTypes(a), actionTypes(b)) {
			t.Errorf("%s: rewording changed %v -> %v", id, actionTypes(a), actionTypes(b))
		}
	}
}

// State transition: resolving a WAL incident drops its candidates.
func TestResolvedWALIncidentHasNoCandidates(t *testing.T) {
	incident := rcaShapedIncident(signalid.ReplicationLagIncreasing, "lag")
	resolved := evidenceAnchor.Add(time.Hour)
	incident.ResolvedAt = &resolved

	got := ProjectIncident(incident)

	if got.State != StateResolved || len(got.ActionCandidates) != 0 {
		t.Fatalf("resolved WAL incident: state=%q candidates=%v", got.State,
			actionTypes(got))
	}
}

// Concurrent access: projection reads only the shared table.
func TestConcurrentPlaybookProjectionIsDeterministic(t *testing.T) {
	incident := rcaShapedIncident(signalid.WALGrowthSpike, "WAL spiked")
	want := actionTypes(ProjectIncident(incident))
	var wg sync.WaitGroup
	results := make([][]string, 16)
	for k := range results {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			results[k] = actionTypes(ProjectIncident(incident))
		}(k)
	}
	wg.Wait()
	for k, got := range results {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("projection %d = %v, want %v", k, got, want)
		}
	}
}
