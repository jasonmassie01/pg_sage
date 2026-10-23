package decide

import (
	"context"
	"fmt"
	"time"
)

// D9: the pending-approval budget (blocked, agent_budget) and the hourly
// request rate (parked, agent_rate, with retry_after). They limit what an
// agent may put in front of people, so reads and approved requests are not
// counted.
func (d *Decider) d9(ctx context.Context, s *state) (Verdict, bool) {
	if s.req.OperatorApproved || s.req.Capability == CapRead {
		return Verdict{}, false
	}
	if d.cfg.Pending != nil && s.req.PrincipalID != "" {
		pending, err := d.cfg.Pending(ctx, s.req.PrincipalID)
		if err != nil {
			return unavailable("D9", "counting pending approvals", err), true
		}
		if pending >= d.cfg.MaxPendingPerPrincipal {
			return deny(ReasonBudget, "D9", fmt.Sprintf("%d approvals are already "+
				"pending for this agent (limit %d)", pending, d.cfg.MaxPendingPerPrincipal),
				"people decide the pending approvals first"), true
		}
	}
	if wait, ok := d.admit(s.req.PrincipalID); !ok {
		v := deny(ReasonRate, "D9", fmt.Sprintf("more than %d requests in an hour; "+
			"retry_after=%ds", d.cfg.MaxRequestsPerHour, int(wait.Seconds())), "")
		v.Park, v.RetryAfter = true, wait
		return v, true
	}
	return Verdict{}, false
}

// admit records one request for principal unless the last hour is full,
// in which case it returns how long until the oldest request leaves it.
func (d *Decider) admit(principal string) (time.Duration, bool) {
	key := principal
	if key == "" {
		key = "stdio:unbound"
	}
	now := d.cfg.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	recent := d.hits[key][:0]
	for _, at := range d.hits[key] {
		if now.Sub(at) < time.Hour {
			recent = append(recent, at)
		}
	}
	if len(recent) >= d.cfg.MaxRequestsPerHour {
		d.hits[key] = recent
		return recent[0].Add(time.Hour).Sub(now), false
	}
	d.hits[key] = append(recent, now)
	return 0, true
}

// D10: a brokered use of a grant needs an unexpired lease in the registry,
// even if the database grant still exists.
func (d *Decider) d10(ctx context.Context, s *state) (Verdict, bool) {
	if s.req.GrantID == "" {
		return Verdict{}, false
	}
	fix := "request the capability again; a person approves the new grant"
	if d.cfg.Leases == nil {
		return deny(ReasonLeaseExpired, "D10", "no lease registry is configured, so grant "+
			s.req.GrantID+" has no lease", fix), true
	}
	active, err := d.cfg.Leases.LeaseActive(ctx, s.req.PrincipalID, s.req.GrantID,
		s.req.Database)
	if err != nil {
		return unavailable("D10", "reading the grant lease", err), true
	}
	if !active {
		return deny(ReasonLeaseExpired, "D10", "the lease of grant "+s.req.GrantID+
			" has expired", fix), true
	}
	return Verdict{}, false
}
