package cases

import "github.com/pg-sage/sidecar/internal/signalid"

// incidentPlaybook names the candidate set an incident's signals select.
type incidentPlaybook string

const (
	playbookNone        incidentPlaybook = "none"
	playbookLock        incidentPlaybook = "lock"
	playbookConnections incidentPlaybook = "connection_exhaustion"
	playbookWAL         incidentPlaybook = "wal_replication"
	playbookStandby     incidentPlaybook = "standby_conflicts"
	playbookRunaway     incidentPlaybook = "runaway_query"
	// playbookVacuumBlocked splits on RCA's branch: a structured xmin
	// holder on the chain gets the freeze-blocker diagnostic, the
	// falling-behind branch (no holder) the vacuum-pressure playbook.
	playbookVacuumBlocked  incidentPlaybook = "vacuum_blocked"
	playbookVacuumPressure incidentPlaybook = "vacuum_pressure"
	playbookFreeze         incidentPlaybook = "freeze_blockers"
)

// signalPlaybooks is the one signal -> playbook decision for every id in
// signalid.All() (playbooks-02); "none" is a deliberate entry, and an id
// missing here selects nothing. playbookLock defers to the idle-tx and
// lock predicate (playbooks-01): when it does not hold, nothing is offered.
var signalPlaybooks = map[string]incidentPlaybook{
	signalid.ConnectionsHigh:          playbookConnections,
	signalid.IdleInTxElevated:         playbookLock,
	signalid.CacheHitRatioDrop:        playbookNone,
	signalid.ReplicationLagIncreasing: playbookWAL,
	signalid.VacuumBlocked:            playbookVacuumBlocked,
	signalid.LockContention:           playbookLock,
	signalid.WALGrowthSpike:           playbookWAL,
	signalid.OrphanedPreparedTx:       playbookFreeze,

	signalid.LogDeadlockDetected:        playbookNone,
	signalid.LogConnectionRefused:       playbookConnections,
	signalid.LogOutOfMemory:             playbookNone,
	signalid.LogDiskFull:                playbookNone,
	signalid.LogPanicServerCrash:        playbookNone,
	signalid.LogDataCorruption:          playbookNone,
	signalid.LogTxidWraparoundWarning:   playbookFreeze,
	signalid.LogArchiveFailed:           playbookNone,
	signalid.LogTempFileCreated:         playbookNone,
	signalid.LogCheckpointTooFrequent:   playbookNone,
	signalid.LogLockTimeout:             playbookNone,
	signalid.LogStatementTimeout:        playbookRunaway,
	signalid.LogReplicationConflict:     playbookStandby,
	signalid.LogWALSegmentRemoved:       playbookWAL,
	signalid.LogAutovacuumCancel:        playbookVacuumPressure,
	signalid.LogReplicationSlotInactive: playbookWAL,
	signalid.LogAuthenticationFailure:   playbookNone,
	signalid.LogSlowQuery:               playbookRunaway,
	signalid.LogInternalError:           playbookNone,
	// rca-02: an XID or MultiXact wraparound stop is the freeze horizon's
	// outage; the read-only freeze-blocker diagnostic finds its holder.
	signalid.LogWraparoundOutage:           playbookFreeze,
	signalid.LogMultiXactWraparoundWarning: playbookFreeze,
	signalid.LogMultiXactWraparoundOutage:  playbookFreeze,

	signalid.SRECheckpointStorm:   playbookNone,
	signalid.SRETempFileExplosion: playbookNone,
	signalid.SRELWLockContention:  playbookNone,
}

// playbookPrecedence decides between the playbooks of an incident with
// several signals, independent of the order RCA listed them. playbookLock
// is absent: its predicate is checked before the table is consulted.
var playbookPrecedence = []incidentPlaybook{
	playbookRunaway, playbookConnections, playbookStandby,
	playbookWAL, playbookVacuumBlocked, playbookVacuumPressure, playbookFreeze,
}

// playbookForSignals is the highest-precedence playbook any signal selects.
func playbookForSignals(signals []string) incidentPlaybook {
	selected := map[incidentPlaybook]bool{}
	for _, signal := range signals {
		selected[signalPlaybooks[signal]] = true
	}
	for _, playbook := range playbookPrecedence {
		if selected[playbook] {
			return playbook
		}
	}
	return playbookNone
}

// incidentPlaybookCandidates builds the candidates of every playbook but
// the lock one, which actionCandidatesForIncident resolves first.
func incidentPlaybookCandidates(i SourceIncident, p incidentPlaybook) []ActionCandidate {
	switch p {
	case playbookRunaway:
		return runawayQueryPlaybookCandidates(i)
	case playbookConnections:
		return []ActionCandidate{diagnosticIncidentCandidate(
			i, "diagnose_connection_exhaustion",
			[]string{"identify connection pressure by role, app, and state"})}
	case playbookStandby:
		return []ActionCandidate{diagnosticIncidentCandidate(
			i, "diagnose_standby_conflicts",
			[]string{"verify standby conflict type and replay pressure"})}
	case playbookWAL:
		return []ActionCandidate{diagnosticIncidentCandidate(
			i, "diagnose_wal_replication",
			[]string{"verify WAL retention, slot status, and replay lag"})}
	case playbookVacuumBlocked:
		if hasStructuredHolder(i) {
			return []ActionCandidate{freezeBlockersIncidentCandidate(i)}
		}
		return autovacuumIncidentCandidates(i)
	case playbookVacuumPressure:
		return []ActionCandidate{vacuumPressureDiagnostic(i)}
	case playbookFreeze:
		return []ActionCandidate{freezeBlockersIncidentCandidate(i)}
	default:
		return nil
	}
}

// hasStructuredHolder reports whether RCA recorded the xmin holder of a
// vacuum_blocked incident (its holder branches); the falling-behind
// branch, and rows written before holders were recorded, carry none.
func hasStructuredHolder(i SourceIncident) bool {
	for _, link := range i.CausalChain {
		if link.Signal == signalid.VacuumBlocked && link.Blocker != nil {
			return true
		}
	}
	return false
}

func freezeBlockersIncidentCandidate(i SourceIncident) ActionCandidate {
	return diagnosticIncidentCandidate(i, "diagnose_freeze_blockers",
		verificationPlanForAction("diagnose_freeze_blockers"))
}

// vacuumPressureDiagnostic reads the database's dead-tuple leaders, xmin
// holders and running vacuums; the affected table is in the case evidence.
func vacuumPressureDiagnostic(i SourceIncident) ActionCandidate {
	return diagnosticIncidentCandidate(i, "diagnose_vacuum_pressure",
		[]string{"identify blockers, dead tuples, and oldest xmin holders"})
}
