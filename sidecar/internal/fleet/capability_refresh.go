package fleet

import (
	"context"
	"time"
)

// DefaultCapabilityRefreshInterval is how often a runtime recollects its
// provider capabilities, so grants and CREATE EXTENSION made after start
// reach readiness without a restart.
const DefaultCapabilityRefreshInterval = 15 * time.Minute

// RefreshProviderCapabilities recollects inst's provider capabilities from
// its pool and stores them under the instance lock. The pending provider a
// re-probe recorded is kept. A refresh cut short by ctx stores nothing, so
// shutdown never replaces the last evidence with unknowns.
func RefreshProviderCapabilities(
	ctx context.Context, inst *DatabaseInstance, provider string, explain FamilyExplainer,
) {
	if inst == nil {
		return
	}
	caps := CollectProviderCapabilities(ctx, inst.Pool, provider, explain)
	if ctx.Err() != nil {
		return
	}
	inst.UpdateStatus(func(status *InstanceStatus) {
		caps.PendingProvider = status.Capabilities.PendingProvider
		caps.Blockers = readinessBlockers(caps)
		caps.ReadyForAutoSafe = readyForAutoSafe(caps)
		status.Capabilities = caps
	})
}

// CapabilityRefresher refreshes one instance's capabilities on an interval
// and on request (a config reload).
type CapabilityRefresher struct {
	inst     *DatabaseInstance
	provider string
	explain  FamilyExplainer
	every    time.Duration
	kick     chan struct{}
}

// NewCapabilityRefresher refreshes inst every interval; a non-positive
// interval takes DefaultCapabilityRefreshInterval.
func NewCapabilityRefresher(
	inst *DatabaseInstance, provider string, explain FamilyExplainer, every time.Duration,
) *CapabilityRefresher {
	if every <= 0 {
		every = DefaultCapabilityRefreshInterval
	}
	return &CapabilityRefresher{inst: inst, provider: provider, explain: explain,
		every: every, kick: make(chan struct{}, 1)}
}

// Request asks for a refresh without blocking; requests made while one is
// pending coalesce into it.
func (r *CapabilityRefresher) Request() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run refreshes on every tick and request until ctx ends.
func (r *CapabilityRefresher) Run(ctx context.Context) {
	ticker := time.NewTicker(r.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.kick:
		}
		RefreshProviderCapabilities(ctx, r.inst, r.provider, r.explain)
	}
}

// RequestCapabilityRefresh asks every registered runtime that runs a
// refresher to recollect its capabilities, as after a config reload.
func (m *DatabaseManager) RequestCapabilityRefresh() {
	if m == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, inst := range m.instances {
		if inst != nil && inst.CapabilityRefresh != nil {
			inst.CapabilityRefresh()
		}
	}
}
