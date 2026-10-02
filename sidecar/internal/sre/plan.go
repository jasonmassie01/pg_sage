package sre

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Probe plans: the fixed, deterministic probes each trigger family runs,
// step by step. A sampling step waits the sample interval first, so rates
// and growth compare two observations. Every plan also reads pg_sage's
// own recent actions ("did pg_sage cause this?", CHECK-38). The largest
// plan runs 7 probes, under the 12-probe ceiling.

type probeCall struct {
	id   probes.ID
	args probes.Args
}

type planStep struct {
	sample bool
	gap    int // sample intervals a sampling step waits; 0 means 1
	calls  []probeCall
}

// waits is how many sample intervals the step waits before its probes.
func (s planStep) waits() int { return max(s.gap, 1) }

func calls(ids ...probes.ID) []probeCall {
	out := make([]probeCall, 0, len(ids))
	for _, id := range ids {
		out = append(out, probeCall{id: id})
	}
	return out
}

// planFor returns the probe plan of a trigger family.
func planFor(kind TriggerKind, actionWindow time.Duration) ([]planStep, bool) {
	actions := probeCall{id: probes.SageActions, args: probes.Args{Window: actionWindow}}
	with := func(ids ...probes.ID) []probeCall { return append(calls(ids...), actions) }
	switch kind {
	case TriggerLock:
		return []planStep{{calls: with(probes.LockGraph, probes.PreparedXacts,
			probes.LongTransactions)}}, true
	case TriggerConnections:
		return []planStep{{calls: with(probes.ConnectionSaturation, probes.LockGraph)},
			{sample: true, calls: calls(probes.ConnectionSaturation)}}, true
	case TriggerWAL:
		return []planStep{{calls: with(probes.ReplicationSlots, probes.WALCheckpoint,
			probes.Archiver)}, {sample: true, calls: calls(probes.ReplicationSlots,
			probes.WALCheckpoint, probes.Archiver)}}, true
	case TriggerPlan:
		return []planStep{{calls: with(probes.PlanRegressions)}}, true
	}
	return planM6(kind, actions)
}

// observations decodes stored evidence into cited probe results. Numbers
// decode as json.Number so 64-bit identities (queryid) stay exact.
func observations(evidence []Evidence) ([]causal.Observation, error) {
	out := make([]causal.Observation, 0, len(evidence))
	for _, e := range evidence {
		var res probes.Result
		dec := json.NewDecoder(bytes.NewReader(e.Payload))
		dec.UseNumber()
		if err := dec.Decode(&res); err != nil {
			return nil, fmt.Errorf("evidence %s: %w", e.ID, err)
		}
		out = append(out, causal.Observation{EvidenceID: string(e.ID), Result: res})
	}
	return out, nil
}

// diagnose runs the investigation family's matcher and adds pg_sage's
// own change as a hypothesis.
func diagnose(inv Investigation, obs []causal.Observation) causal.Diagnosis {
	obs = currentObservations(obs)
	var d causal.Diagnosis
	switch inv.TriggerKind {
	case TriggerLock:
		d = causal.DiagnoseLock(obs, nil)
	case TriggerConnections:
		d = causal.DiagnoseConnections(obs)
	case TriggerWAL:
		d = causal.DiagnoseWAL(obs)
	case TriggerPlan:
		d = planDiagnosis(obs, inv.Subject)
	default:
		d = diagnoseM6(inv.TriggerKind, obs)
	}
	return causal.WithSelfActions(d, obs)
}

// seriesProbes are compared across samples. Every other probe is a
// one-shot observation: when it ran again (a model-proposed probe), its
// newest result supersedes the older ones.
var seriesProbes = map[probes.ID]bool{probes.ConnectionSaturation: true,
	probes.ReplicationSlots: true, probes.WALCheckpoint: true, probes.Archiver: true,
	probes.CheckpointActivity: true, probes.TempFileActivity: true,
	probes.TempFileHolders: true, probes.TempSpillStatements: true,
	probes.ReplicationLag: true, probes.StandbyReplayState: true, probes.LWLockWaits: true}

// currentObservations keeps every sample of a series probe and only the
// newest (last stored) observation of each one-shot probe, in order.
func currentObservations(obs []causal.Observation) []causal.Observation {
	latest := map[probes.ID]int{}
	for i, o := range obs {
		latest[o.Result.ProbeID] = i
	}
	out := make([]causal.Observation, 0, len(obs))
	for i, o := range obs {
		if seriesProbes[o.Result.ProbeID] || latest[o.Result.ProbeID] == i {
			out = append(out, o)
		}
	}
	return out
}

// planDiagnosis is the diagnosis of the triggering query, or an
// inconclusive one saying it did not regress (or why it cannot be read).
func planDiagnosis(obs []causal.Observation, subject string) causal.Diagnosis {
	ds := causal.DiagnosePlan(obs)
	for _, d := range ds {
		if d.Subject == subject {
			return d
		}
	}
	if len(ds) == 1 && len(ds[0].Missing) > 0 {
		return ds[0]
	}
	return causal.Diagnosis{Family: causal.FamilyPlanRegression,
		GraphVersion: causal.GraphVersion, Subject: subject,
		Reason: fmt.Sprintf("%s did not regress by %sx in the window", subject,
			probes.FormatValue(causal.RegressionRatio))}
}

// conclusionOf turns a diagnosis into the persisted conclusion: root
// first, then contributing, unproven and ruled-out hypotheses.
func conclusionOf(d causal.Diagnosis) Conclusion {
	c := Conclusion{State: StateInconclusive, Summary: summaryOf(d)}
	var ordered []causal.Hypothesis
	if d.Root != nil && d.Conclusive {
		c.State = StateConcluded
		ordered = append(ordered, *d.Root)
	}
	ordered = append(ordered, d.Contributing...)
	ordered = append(ordered, d.Alternatives...)
	ordered = append(ordered, d.RuledOut...)
	for _, h := range ordered {
		c.Hypotheses = append(c.Hypotheses, recordOf(d, h))
	}
	return c
}

func summaryOf(d causal.Diagnosis) Summary {
	s := Summary{Family: string(d.Family), GraphVersion: d.GraphVersion,
		Subject: d.Subject, Conclusive: d.Conclusive && d.Root != nil, Reason: d.Reason,
		Observed: facts(d.Observed)}
	if s.Conclusive {
		s.Root = string(d.Root.Node)
	}
	for _, m := range d.Missing {
		s.Missing = append(s.Missing, MissingEvidence{ProbeID: string(m.ProbeID),
			Status: string(m.Status), Reason: m.Reason})
	}
	return s
}

func recordOf(d causal.Diagnosis, h causal.Hypothesis) HypothesisRecord {
	family := string(d.Family)
	if n, ok := causal.NodeByID(h.Node); ok {
		family = string(n.Family)
	}
	return HypothesisRecord{GraphVersion: d.GraphVersion, Family: family,
		Node: string(h.Node), Label: h.Label, Mechanism: h.Mechanism, Subject: h.Subject,
		Status: HypothesisStatus(h.Status), Confidence: h.Confidence,
		Support: facts(h.Support), Contradict: facts(h.Contradict),
		RefutationProbe: h.RefutationProbe, OperatorStep: h.OperatorStep}
}

func facts(fs []causal.Fact) []Fact {
	out := make([]Fact, 0, len(fs))
	for _, f := range fs {
		out = append(out, Fact{EvidenceID: UUID(f.EvidenceID), Text: f.Text})
	}
	return out
}
