package sre

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sage SRE M5 signals: evidence sources outside the SQL catalog (the
// change feed and the SLO status). When wired, every investigation also
// collects them in its first step, so "what changed?" and "are customers
// hurt?" are answered with evidence; the model can never propose them.

// SignalProbe is one signal source: a typed probe result produced in Go.
type SignalProbe struct {
	ID  probes.ID
	Run func(ctx context.Context, args probes.Args) probes.Result
}

func validateSignals(signals []SignalProbe) error {
	seen := map[probes.ID]bool{}
	for _, s := range signals {
		switch {
		case !probes.IsSignal(s.ID):
			return fmt.Errorf("%w: %s is not a signal probe", ErrInvalidRequest, s.ID)
		case s.Run == nil:
			return fmt.Errorf("%w: signal %s has no source", ErrInvalidRequest, s.ID)
		case seen[s.ID]:
			return fmt.Errorf("%w: duplicate signal %s", ErrInvalidRequest, s.ID)
		}
		seen[s.ID] = true
	}
	return nil
}

// signalRunner answers signal probes from their sources and every other
// probe from the catalog runner.
type signalRunner struct {
	base    ProbeRunner
	signals map[probes.ID]SignalProbe
}

func withSignals(base ProbeRunner, signals []SignalProbe) ProbeRunner {
	if len(signals) == 0 {
		return base
	}
	r := signalRunner{base: base, signals: map[probes.ID]SignalProbe{}}
	for _, s := range signals {
		r.signals[s.ID] = s
	}
	return r
}

func (r signalRunner) Run(ctx context.Context, id probes.ID, args probes.Args) probes.Result {
	if s, ok := r.signals[id]; ok {
		return s.Run(ctx, args)
	}
	return r.base.Run(ctx, id, args)
}

func signalIDs(signals []SignalProbe) []probes.ID {
	out := make([]probes.ID, 0, len(signals))
	for _, s := range signals {
		out = append(out, s.ID)
	}
	return out
}

// addSignals is a family's plan with the signal probes added to
// its first step (the change feed over the action window).
func addSignals(plan []planStep, ok bool, window time.Duration,
	signals []probes.ID) ([]planStep, bool) {
	if !ok || len(signals) == 0 {
		return plan, ok
	}
	first := append([]probeCall(nil), plan[0].calls...)
	for _, id := range signals {
		call := probeCall{id: id}
		if id == probes.ChangeFeed {
			call.args = probes.Args{Window: window}
		}
		first = append(first, call)
	}
	plan[0].calls = first
	return plan, true
}

// sloBurnPlan triages an SLO burn: lock, connection and plan evidence
// plus pg_sage's own actions (7 probes, 9 with both signals).
func sloBurnPlan(actions probeCall) []planStep {
	return []planStep{{calls: append(calls(probes.LockGraph, probes.PreparedXacts,
		probes.LongTransactions, probes.ConnectionSaturation, probes.PlanRegressions),
		actions)}}
}

// CustomerImpact is the persisted customer-impact claim.
type CustomerImpact struct {
	State      string   `json:"state"`
	SLO        string   `json:"slo,omitempty"`
	EvidenceID UUID     `json:"evidence_id,omitempty"`
	BurnRate   *float64 `json:"burn_rate,omitempty"`
	Window     string   `json:"window,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

func (c *CustomerImpact) validate() error {
	if c == nil {
		return nil
	}
	switch c.State {
	case causal.ImpactBurning, causal.ImpactNotBurning, causal.ImpactUnknown,
		causal.ImpactNoAppSLO:
	default:
		return fmt.Errorf("%w: customer impact state %q", ErrInvalidRequest, c.State)
	}
	if c.EvidenceID != "" {
		if _, err := ParseUUID(string(c.EvidenceID)); err != nil {
			return err
		}
	}
	for _, f := range []struct{ name, value string }{{"impact slo", c.SLO},
		{"impact window", c.Window}, {"impact reason", c.Reason}} {
		if err := checkText(f.name, f.value, false, 128); err != nil {
			return err
		}
	}
	return nil
}

func customerImpactOf(im *causal.Impact) *CustomerImpact {
	if im == nil {
		return nil
	}
	return &CustomerImpact{State: im.State, SLO: truncateRunes(im.SLO, 128),
		EvidenceID: UUID(im.EvidenceID), BurnRate: im.BurnRate,
		Window: truncateRunes(im.Window, 128), Reason: truncateRunes(im.Reason, 128)}
}
