package slo

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

const ReasonHistoricalIncomplete = "historical_capture_completeness_unknown"
const ReasonSourcePolicyDenied = "source_policy_denied"

// historicalErrorProxy supplies an explicit source diagnosis instead of a
// healthy zero-error proxy. Provider logs may be sampled/delayed and the
// transaction intervals cannot establish matching complete error coverage.
// Application Push/Prometheus SLIs remain available independently.
type historicalErrorProxy struct {
	source probes.HistoricalSource
	cfg    ProxyConfig
}

func NewHistoricalErrorProxy(source probes.HistoricalSource, cfg ProxyConfig) Proxy {
	return &historicalErrorProxy{source, cfg}
}
func (p *historicalErrorProxy) Objective() Objective {
	return newProxyObjective(ProxyErrors, "database proxy: Lakebase historical logs cannot prove a complete aligned server-error rate; use an application SLI", p.cfg)
}
func (p *historicalErrorProxy) Slice(ctx context.Context, _ time.Time, _ History) ProxySlice {
	if p.source == nil {
		return ProxySlice{Reason: ReasonSourceUnavailable}
	}
	for _, id := range []probes.ID{probes.LakebaseLogs, probes.LakebaseDatabase} {
		r := p.source.ReadHistory(ctx, id, probes.Args{Window: 5 * time.Minute})
		if r.Status == probes.StatusError {
			return ProxySlice{Reason: ReasonSourceError}
		}
		if r.Reason == probes.ReasonPolicyDenied || r.Reason == "history_policy_revoked" {
			return ProxySlice{Reason: ReasonSourcePolicyDenied}
		}
		if !r.Status.Usable() && r.Reason != "historical_no_observations_completeness_unknown" {
			return ProxySlice{Reason: ReasonSourceUnavailable}
		}
	}
	return ProxySlice{Reason: ReasonHistoricalIncomplete}
}
