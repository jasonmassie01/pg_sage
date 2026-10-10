package decide

import (
	"context"
	"sync"
	"time"
)

// Decider runs the D-steps. It is safe for concurrent use.
type Decider struct {
	cfg  Config
	mu   sync.Mutex
	hits map[string][]time.Time // D9: request times per principal, last hour
}

// New returns a decider over cfg.
func New(cfg Config) *Decider {
	if cfg.MaxRequestsPerHour <= 0 {
		cfg.MaxRequestsPerHour = DefaultMaxRequestsPerHour
	}
	if cfg.MaxPendingPerPrincipal <= 0 {
		cfg.MaxPendingPerPrincipal = DefaultMaxPendingPerPrincipal
	}
	if cfg.RestoreDrillDays <= 0 {
		cfg.RestoreDrillDays = DefaultRestoreDrillDays
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Decider{cfg: cfg, hits: map[string][]time.Time{}}
}

// Decide runs D1-D10 on req; the first failure decides (§6.2.2).
func (d *Decider) Decide(ctx context.Context, req Request) Verdict {
	return Verdict{Reason: ReasonUnavailable, Step: "D1",
		Detail: "agent governance decision is not implemented yet"}
}
