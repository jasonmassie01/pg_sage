package sre

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Pre-incident (runway) investigations (AI-SRE-SPEC §4 R2): the runway
// monitor opens one when a measured runway crosses its horizon. They are
// read-only like every investigation; a conclusive one carries the
// existing custodian actions (freeze, WAL bound) that address it, with
// the standing gate's verdict explained. The custodian worker executes
// them under the configured autonomy, never the investigation.

// Runway trigger kinds.
const (
	TriggerWraparound TriggerKind = "wraparound_runway"
	TriggerDiskWAL    TriggerKind = "disk_wal_runway"
	TriggerSequence   TriggerKind = "sequence_runway"
)

// DefaultRunwayWindow is the trend window of a runway investigation when
// the coordinator does not set one: the runway monitor's default lookback.
const DefaultRunwayWindow = 6 * time.Hour

// Proposal limits.
const (
	MaxProposals  = 3
	maxTargets    = 4
	adviceTimeout = 10 * time.Second
)

// Runway reports a pre-incident (runway) trigger kind.
func (k TriggerKind) Runway() bool {
	return k == TriggerWraparound || k == TriggerDiskWAL || k == TriggerSequence
}

// runwayPlan is the fixed probe plan of a runway family: the current
// state with the sampled trends and pg_sage's own actions, then a second
// sample of the series it compares.
func runwayPlan(kind TriggerKind, actionWindow, trendWindow time.Duration) ([]planStep,
	bool) {
	actions := probeCall{id: probes.SageActions, args: probes.Args{Window: actionWindow}}
	trends := probeCall{id: probes.RunwayTrendsProbe, args: probes.Args{Window: trendWindow}}
	switch kind {
	case TriggerWraparound:
		cancels := probeCall{id: probes.AutovacuumCancellations,
			args: probes.Args{Window: trendWindow}}
		first := append(calls(probes.XIDRunwayProbe, probes.WraparoundTablesProbe,
			probes.XminHorizon), cancels, trends, actions)
		return []planStep{{calls: first},
			{sample: true, calls: calls(probes.XIDRunwayProbe)}}, true
	case TriggerDiskWAL:
		first := append([]probeCall{trends}, calls(probes.ReplicationSlots,
			probes.WALCheckpoint, probes.Archiver, probes.WALDirectoryProbe)...)
		return []planStep{{calls: append(first, actions)}, {sample: true,
			calls: calls(probes.ReplicationSlots, probes.WALCheckpoint, probes.Archiver)}}, true
	case TriggerSequence:
		first := append(calls(probes.SequenceRunwayProbe), trends, actions)
		return []planStep{{calls: first},
			{sample: true, calls: calls(probes.SequenceRunwayProbe)}}, true
	}
	return nil, false
}

// plan is the probe plan of a trigger kind under this coordinator's
// windows, with the wired signal probes (M5) in its first step.
func (c *Coordinator) plan(kind TriggerKind) ([]planStep, bool) {
	if kind.Runway() {
		plan, ok := runwayPlan(kind, c.cfg.ActionWindow, c.cfg.runwayWindow())
		return addSignals(plan, ok, c.cfg.ActionWindow, c.signals)
	}
	plan, ok := planFor(kind, c.cfg.ActionWindow)
	return addSignals(plan, ok, c.cfg.ActionWindow, c.signals)
}

// runwayWindow is the configured trend window, or the default.
func (c CoordinatorConfig) runwayWindow() time.Duration {
	if c.RunwayWindow == 0 {
		return DefaultRunwayWindow
	}
	return c.RunwayWindow
}

// diagnoseRunway runs the runway family's matcher.
func diagnoseRunway(inv Investigation, obs []causal.Observation) causal.Diagnosis {
	switch inv.TriggerKind {
	case TriggerWraparound:
		return causal.DiagnoseWraparound(obs, inv.Subject)
	case TriggerDiskWAL:
		return causal.DiagnoseDiskWAL(obs)
	default:
		return causal.DiagnoseSequence(obs, inv.Subject)
	}
}

// AdviceRequest asks for the custodian actions that address a concluded
// runway diagnosis.
type AdviceRequest struct {
	Kind    TriggerKind
	Subject string
	Root    string
}

// ActionProposal is an existing custodian action that addresses the
// diagnosis, with the standing gate's verdict explained (not recorded).
// "manual_only" means no custodian action covers it: an operator step.
type ActionProposal struct {
	Feature  string   `json:"feature"`
	Action   string   `json:"action"`
	SQL      string   `json:"sql,omitempty"`
	Targets  []string `json:"targets"`
	Verdict  string   `json:"verdict"`
	Reason   string   `json:"reason,omitempty"`
	RiskTier string   `json:"risk_tier,omitempty"`
}

// ActionAdvisor returns the custodian proposals for a runway diagnosis.
// It never executes and never records a decision.
type ActionAdvisor interface {
	Advise(ctx context.Context, req AdviceRequest) ([]ActionProposal, error)
}

// advise asks the advisor about a conclusive runway diagnosis. A failure
// or an invalid answer is logged and leaves the conclusion without
// proposals: the advisor never fails an investigation.
func (c *Coordinator) advise(ctx context.Context, inv Investigation,
	d causal.Diagnosis) []ActionProposal {
	if c.advisor == nil || !inv.TriggerKind.Runway() || !d.Conclusive || d.Root == nil {
		return nil
	}
	actx, cancel := context.WithTimeout(ctx, adviceTimeout)
	defer cancel()
	ps, err := c.advisor.Advise(actx, AdviceRequest{Kind: inv.TriggerKind,
		Subject: inv.Subject, Root: string(d.Root.Node)})
	if err != nil {
		c.logFn("WARN", "sre: investigation %s: custodian proposal unavailable: %v",
			inv.ID, err)
		return nil
	}
	if err := validateProposals(ps); err != nil {
		c.logFn("WARN", "sre: investigation %s: custodian proposal refused: %v", inv.ID, err)
		return nil
	}
	return ps
}

func validateProposals(ps []ActionProposal) error {
	if len(ps) > MaxProposals {
		return fmt.Errorf("%w: %d proposals over %d", ErrInvalidRequest, len(ps),
			MaxProposals)
	}
	for i, p := range ps {
		if len(p.Targets) > maxTargets {
			return fmt.Errorf("%w: proposal %d has %d targets over %d", ErrInvalidRequest,
				i+1, len(p.Targets), maxTargets)
		}
		fields := []struct {
			name, value string
			required    bool
			max         int
		}{{"proposal feature", p.Feature, true, 64}, {"proposal action", p.Action, false, 512},
			{"proposal sql", p.SQL, false, 1024}, {"proposal verdict", p.Verdict, true, 64},
			{"proposal reason", p.Reason, false, 512}, {"proposal risk", p.RiskTier, false, 32}}
		for _, t := range p.Targets {
			fields = append(fields, struct {
				name, value string
				required    bool
				max         int
			}{"proposal target", t, true, 256})
		}
		for _, f := range fields {
			if err := checkText(f.name, f.value, f.required, f.max); err != nil {
				return fmt.Errorf("proposal %d: %w", i+1, err)
			}
		}
	}
	return nil
}
