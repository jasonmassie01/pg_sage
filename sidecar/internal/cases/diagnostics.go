package cases

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// diagnosticProbes maps each read-only diagnose_* action to the SRE
// catalog probes whose SQL it proposes (playbooks-04). The probes are
// bounded by their row caps, scoped to current_database() and versioned;
// the hand-written queries they replace were unbounded and unscoped, and
// paired a virtualxid waiter with every session.
var diagnosticProbes = map[string][]probes.ID{
	"diagnose_lock_blockers":         {probes.LockGraph},
	"diagnose_runaway_query":         {probes.LongTransactions, probes.TempFileHolders},
	"diagnose_connection_exhaustion": {probes.ConnectionSaturation},
	"diagnose_wal_replication":       {probes.ReplicationSlots, probes.ReplicationLag},
	"diagnose_standby_conflicts":     {probes.StandbyReplayState},
	"diagnose_freeze_blockers":       {probes.XminHorizon, probes.WraparoundTablesProbe},
	"diagnose_vacuum_pressure": {probes.StatTables, probes.XminHorizon,
		probes.VacuumProgress},
}

// diagnosticScripts is every diagnostic's script, rendered once. A mapped
// probe without display SQL is a catalog bug: the package refuses to
// start rather than propose a diagnostic it cannot render.
var diagnosticScripts = mustDiagnosticScripts()

func mustDiagnosticScripts() map[string]string {
	out := make(map[string]string, len(diagnosticProbes))
	for action, ids := range diagnosticProbes {
		stmts := make([]string, 0, len(ids))
		for _, id := range ids {
			sql, err := probes.DisplaySQL(id)
			if err != nil {
				panic("case diagnostic " + action + ": " + err.Error())
			}
			stmts = append(stmts, sql)
		}
		out[action] = strings.Join(stmts, ";\n\n") + ";"
	}
	return out
}

// diagnosticSQL is the read-only script a diagnose_* action proposes; it
// is empty for any other action.
func diagnosticSQL(actionType string) string { return diagnosticScripts[actionType] }

// runawayVerificationPlan is the runaway diagnostic's plan; it names the
// role its temp file statement needs.
func runawayVerificationPlan() []string {
	return []string{"confirm query age, wait state, and temp spill evidence",
		tempFileHoldersRoleNote}
}

// tempFileHoldersRoleNote names the role pg_ls_tmpdir needs, which
// managed providers grant through pg_monitor only.
const tempFileHoldersRoleNote = "temp file holders read pg_ls_tmpdir(), which needs " +
	"the pg_monitor role (or superuser); without it that statement fails with " +
	"permission denied"
