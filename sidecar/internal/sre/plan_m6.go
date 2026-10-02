package sre

import (
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// M6 reactive-family probe plans. A checkpoint storm is compared over
// six sample intervals (30 s by default): a storm at PostgreSQL's
// "too frequently" threshold requests one checkpoint every 30 s. LWLock
// waits are sampled four times; temp files and replication twice. The
// largest plan runs 7 probes, leaving room for the model turn's one.
func planM6(kind TriggerKind, actions probeCall) ([]planStep, bool) {
	with := func(ids ...probes.ID) []probeCall { return append(calls(ids...), actions) }
	switch kind {
	case TriggerCheckpoint:
		return []planStep{{calls: with(probes.CheckpointActivity)},
			{sample: true, gap: 3, calls: calls(probes.CheckpointActivity)},
			{sample: true, gap: 3, calls: calls(probes.CheckpointActivity)}}, true
	case TriggerTempFiles:
		temp := []probes.ID{probes.TempFileActivity, probes.TempFileHolders,
			probes.TempSpillStatements}
		return []planStep{{calls: with(temp...)}, {sample: true, calls: calls(temp...)}},
			true
	case TriggerReplicationLag:
		repl := []probes.ID{probes.ReplicationLag, probes.StandbyReplayState,
			probes.WALCheckpoint}
		return []planStep{{calls: with(repl...)}, {sample: true, calls: calls(repl...)}},
			true
	case TriggerLWLock:
		sample := planStep{sample: true, calls: calls(probes.LWLockWaits)}
		return []planStep{{calls: with(probes.LWLockWaits)}, sample, sample, sample}, true
	}
	return nil, false
}

// diagnoseM6 runs an M6 family's matcher; an unknown kind is an empty,
// inconclusive diagnosis.
func diagnoseM6(kind TriggerKind, obs []causal.Observation) causal.Diagnosis {
	switch kind {
	case TriggerCheckpoint:
		return causal.DiagnoseCheckpoint(obs)
	case TriggerTempFiles:
		return causal.DiagnoseTempFiles(obs)
	case TriggerReplicationLag:
		return causal.DiagnoseReplicationLag(obs)
	case TriggerLWLock:
		return causal.DiagnoseLWLock(obs)
	}
	return causal.Diagnosis{GraphVersion: causal.GraphVersion,
		Reason: "no causal-graph family for trigger " + string(kind)}
}
