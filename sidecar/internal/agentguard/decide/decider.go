package decide

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
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

// state is one decision in progress.
type state struct {
	req     Request
	p       agentguard.Principal
	access  agentguard.Access
	profile *Profile
	env     envbind.Env
	level   int
}

type step func(d *Decider, ctx context.Context, s *state) (Verdict, bool)

// steps are D1-D10 in order (D8 is a cap, applied with the level).
var steps = []step{(*Decider).d1, (*Decider).d2, (*Decider).d3, (*Decider).d4,
	(*Decider).d5, (*Decider).d6, (*Decider).d7, (*Decider).d9, (*Decider).d10}

// Decide runs D1-D10 on req; the first failure decides (§6.2.2). A
// narrowing request passes: it only takes access away, and containment is
// never blocked by the agent's own state (§6.2.4).
func (d *Decider) Decide(ctx context.Context, req Request) Verdict {
	req = normalize(req)
	if req.Narrowing {
		return Verdict{Allowed: true, MaxLevel: 3, Capability: req.Capability}
	}
	s, v, ok := d.load(ctx, req)
	if !ok {
		return v
	}
	for _, run := range steps {
		if v, stop := run(d, ctx, s); stop {
			v.Env, v.Capability, v.Principal = s.env, req.Capability, s.p
			return v
		}
	}
	return Verdict{Allowed: true, MaxLevel: s.finalLevel(), Env: s.env,
		Capability: req.Capability, Principal: s.p}
}

func normalize(req Request) Request {
	if req.Kind == "" {
		req.Kind = KindFor(req.Tool)
	}
	if req.Capability == "" {
		req.Capability = CapabilityFor(req.Tool, "")
	}
	return req
}

// load reads the principal (D1's input). "" is the unbound stdio agent:
// active, unsponsored, with no environment ceiling of its own.
func (d *Decider) load(ctx context.Context, req Request) (*state, Verdict, bool) {
	s := &state{req: req}
	if req.PrincipalID == "" {
		s.p = agentguard.Principal{Name: "stdio", Status: agentguard.StatusActive,
			EnvCeiling: agentguard.EnvProd}
	} else {
		if d.cfg.Principals == nil {
			return nil, deny(ReasonUnavailable, "D1", "agent governance has no "+
				"principal store (no control database)", ""), false
		}
		p, err := d.cfg.Principals.Get(ctx, req.PrincipalID)
		switch {
		case errors.Is(err, agentguard.ErrNotFound):
			return nil, deny(agentguard.ReasonRetired, "D1", fmt.Sprintf(
				"principal %s does not exist", req.PrincipalID), ""), false
		case err != nil:
			return nil, unavailable("D1", "loading the principal", err), false
		}
		s.p = p
	}
	s.access = agentguard.ToolAccess(s.p, req.Kind)
	s.level = s.access.MaxLevel
	if pr, ok := d.profileOf(s.p); ok {
		s.profile = &pr
	}
	return s, Verdict{}, true
}

func (d *Decider) profileOf(p agentguard.Principal) (Profile, bool) {
	if d.cfg.Profiles == nil || p.Profile == "" {
		return Profile{}, false
	}
	return d.cfg.Profiles.Profile(p.Profile)
}

// finalLevel applies G1's ledger: every class but read at L2, since every
// grant is operator-approved. D8 (taint caps at L2) is the core's
// ToolAccess cap, already in s.level.
func (s *state) finalLevel() int {
	level := s.level
	if s.req.Capability != CapRead {
		level = min(level, 2)
	}
	return level
}

func deny(reason agentguard.Reason, step, detail, fix string) Verdict {
	return Verdict{Reason: reason, Step: step, Detail: detail, Fix: fix}
}

func unavailable(step, what string, err error) Verdict {
	return deny(ReasonUnavailable, step, fmt.Sprintf("agent governance failed %s: %v",
		what, err), "check the control database connection")
}
