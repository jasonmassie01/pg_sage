package cases

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/signalid"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// playbooks-04: every diagnose_* candidate proposes the SRE catalog's
// bounded, database-scoped probe SQL instead of hand-written queries.

const probeIdentityPrefix = "/* pg_sage sre:"

// wantDiagnosticProbes is the plan's action -> probe mapping.
var wantDiagnosticProbes = map[string][]probes.ID{
	"diagnose_lock_blockers":         {probes.LockGraph},
	"diagnose_runaway_query":         {probes.LongTransactions, probes.TempFileHolders},
	"diagnose_connection_exhaustion": {probes.ConnectionSaturation},
	"diagnose_wal_replication":       {probes.ReplicationSlots, probes.ReplicationLag},
	"diagnose_standby_conflicts":     {probes.StandbyReplayState},
	"diagnose_freeze_blockers":       {probes.XminHorizon, probes.WraparoundTablesProbe},
	"diagnose_vacuum_pressure": {probes.StatTables, probes.XminHorizon,
		probes.VacuumProgress},
}

func TestDiagnosticProbesMatchThePlan(t *testing.T) {
	if !reflect.DeepEqual(diagnosticProbes, wantDiagnosticProbes) {
		t.Fatalf("diagnosticProbes = %v, want %v", diagnosticProbes, wantDiagnosticProbes)
	}
}

// diagnosticStatements splits a diagnostic script into its statements.
func diagnosticStatements(script string) []string {
	parts := strings.Split(strings.TrimSuffix(script, ";"), ";\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// Each script is exactly its probes' display SQL, in order.
func TestDiagnosticSQLIsTheProbesDisplaySQL(t *testing.T) {
	for action, ids := range wantDiagnosticProbes {
		script := diagnosticSQL(action)
		if !strings.HasSuffix(script, ";") {
			t.Fatalf("%s script is not terminated: %q", action, script)
		}
		stmts := diagnosticStatements(script)
		if len(stmts) != len(ids) {
			t.Fatalf("%s script has %d statements, want %d", action, len(stmts), len(ids))
		}
		for n, id := range ids {
			want, err := probes.DisplaySQL(id)
			if err != nil {
				t.Fatalf("DisplaySQL(%s): %v", id, err)
			}
			if stmts[n] != want {
				t.Fatalf("%s statement %d is not %s's display SQL:\n%s", action, n, id,
					stmts[n])
			}
		}
	}
}

// Nil/empty: an action without probes renders no SQL, never a guess.
func TestDiagnosticSQLOfUnknownActionIsEmpty(t *testing.T) {
	for _, action := range []string{"", "vacuum_table", "diagnose_nothing"} {
		if got := diagnosticSQL(action); got != "" {
			t.Fatalf("diagnosticSQL(%q) = %q, want empty", action, got)
		}
	}
}

// everyDiagnosticCandidate projects every incident signal (with and
// without a structured holder) and every finding playbook.
func everyDiagnosticCandidate() []ActionCandidate {
	var out []ActionCandidate
	for _, info := range signalid.All() {
		signal := info.ID
		plain := rcaShapedIncident(signal, "incident", "public.orders")
		holder := rcaShapedIncident(signal, "incident", "public.orders")
		holder.CausalChain[0].Blocker = &IncidentBlocker{State: "inactive"}
		for _, inc := range []SourceIncident{plain, holder} {
			out = append(out, ProjectIncident(inc).ActionCandidates...)
		}
	}
	for _, category := range []string{"inactive_slot", "replication_lag", "runaway_query"} {
		got := projectFinding(SourceFinding{ID: "f-" + category, DatabaseName: "prod",
			Category: category, Severity: SeverityWarning, ObjectIdentifier: "x",
			ObservedAt: evidenceAnchor})
		out = append(out, got.ActionCandidates...)
	}
	lock := rcaShapedIncident(signalid.LockContention, "pid 7 blocked by pid 9")
	out = append(out, ProjectIncident(lock).ActionCandidates...)
	f := SourceFinding{ObjectIdentifier: "public.orders", ObservedAt: evidenceAnchor}
	// The finding-side blocked-vacuum candidate was removed as unproduced
	// (vacuum branch); diagnose_vacuum_pressure comes from its incident path.
	pressure := rcaShapedIncident(signalid.LogAutovacuumCancel, "autovacuum task canceled")
	return append(out, freezeDiagnosticCandidate(f), vacuumPressureDiagnostic(pressure))
}

// CHECK-03: every diagnose_* ProposedSQL starts with the probe identity
// comment and is exactly the mapped script.
func TestEveryDiagnosticCandidateProposesProbeSQL(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range everyDiagnosticCandidate() {
		if !strings.HasPrefix(c.ActionType, "diagnose_") {
			continue
		}
		seen[c.ActionType] = true
		if !strings.HasPrefix(c.ProposedSQL, probeIdentityPrefix) {
			t.Fatalf("%s ProposedSQL does not start with %q: %.80s", c.ActionType,
				probeIdentityPrefix, c.ProposedSQL)
		}
		if c.ProposedSQL != diagnosticSQL(c.ActionType) {
			t.Fatalf("%s ProposedSQL is not its probe script", c.ActionType)
		}
	}
	var got []string
	for action := range seen {
		got = append(got, action)
	}
	sort.Strings(got)
	var want []string
	for action := range wantDiagnosticProbes {
		want = append(want, action)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostic actions reached = %v, want %v", got, want)
	}
}

// Managed providers: pg_ls_tmpdir needs pg_monitor, and the runaway
// candidate says so.
func TestRunawayDiagnosticNamesThePgMonitorRole(t *testing.T) {
	got := ProjectIncident(rcaShapedIncident(signalid.LogSlowQuery, "slow query"))
	c := got.ActionCandidates[0]
	if c.ActionType != "diagnose_runaway_query" {
		t.Fatalf("first candidate = %s", c.ActionType)
	}
	if !strings.Contains(strings.Join(c.VerificationPlan, " "), "pg_monitor") {
		t.Fatalf("runaway verification plan does not name pg_monitor: %v",
			c.VerificationPlan)
	}
}

// Invalid input: an object name never reaches diagnostic SQL (the
// vacuum-pressure script once quoted the finding's table into ::regclass,
// which failed when the table was gone).
func TestVacuumPressureDiagnosticTakesNoObjectText(t *testing.T) {
	hostile := rcaShapedIncident(signalid.LogAutovacuumCancel, "autovacuum canceled",
		"public.x'); DROP TABLE t; --")
	c := vacuumPressureDiagnostic(hostile)
	if c.ProposedSQL != diagnosticSQL("diagnose_vacuum_pressure") {
		t.Fatalf("vacuum pressure SQL depends on the object: %s", c.ProposedSQL)
	}
	if strings.Contains(c.ProposedSQL, "DROP TABLE") ||
		strings.Contains(c.ProposedSQL, "::regclass") {
		t.Fatalf("object text reached the diagnostic SQL: %s", c.ProposedSQL)
	}
}
