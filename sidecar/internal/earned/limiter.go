package earned

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Binding is one database's downgrade signal sources.
type Binding struct {
	Database    string
	Budget      BudgetSource
	HA          HASource
	Concurrency ConcurrencySource
}

// Limiter is the ledger as one database's gate sees it
// (policy.AutonomyLimiter).
type Limiter struct {
	svc *Service
	b   Binding

	mu   sync.Mutex
	last map[pairKey]string
	// overrides are the mandatory deadline overrides already recorded,
	// by pair, targets and deadline, until the deadline passes.
	overrides map[string]time.Time
}

// Limiter binds the ledger to one database's signals.
func (s *Service) Limiter(b Binding) *Limiter {
	return &Limiter{svc: s, b: b, last: map[pairKey]string{},
		overrides: map[string]time.Time{}}
}

var _ policy.AutonomyLimiter = (*Limiter)(nil)

// Limit is the effective level for req: the granted level, capped by the
// class and the contract's reversibility, by the level the evidence still
// supports, and by L1 while a downgrade signal holds. Signals are read
// only for a pair granted L2 or more; a ledger read failure is an error
// (the gate then fails closed).
func (l *Limiter) Limit(ctx context.Context, req policy.ActionRequest) (
	policy.AutonomyLimit, error) {
	f, c := Family(strings.TrimSpace(req.IncidentFamily)), ClassFor(req)
	st, err := l.svc.Granted(ctx, f, c)
	if err != nil {
		return policy.AutonomyLimit{}, err
	}
	level := MinLevel(st.Level, effectiveCap(st, c), contractCap(req))
	if !Applicable(f, c) {
		level = MinLevel(level, L1)
	}
	out := policy.AutonomyLimit{Granted: int(st.Level), Class: string(c)}
	if st.Level < L2 {
		out.Level = int(level)
		return out, nil
	}
	// A carried-over level was granted by policy, not earned by evidence,
	// so it does not decay with the evidence.
	if level >= L2 && st.Provenance != ProvenanceCarriedOver {
		supported, err := l.svc.supportedLevel(ctx, f, c)
		if err != nil {
			return policy.AutonomyLimit{}, err
		}
		level = MinLevel(level, supported)
	}
	downs := l.requestDowngrades(ctx, req, f)
	l.noteTransition(ctx, f, c, downs)
	if len(downs) > 0 {
		level = MinLevel(level, L1)
		out.Downgraded = true
		for _, d := range downs {
			out.Reasons = append(out.Reasons, d.Reason)
		}
	}
	out.Level = int(level)
	return out, nil
}

func contractCap(req policy.ActionRequest) Level {
	if req.Contract == nil {
		return L1
	}
	return RollbackCap(req.Contract.RollbackClass)
}

// requestDowngrades evaluates every CHECK-40 signal for req. A source
// that cannot answer is itself a downgrade (fail closed).
func (l *Limiter) requestDowngrades(ctx context.Context, req policy.ActionRequest,
	f Family) []Downgrade {
	out := l.databaseDowngrades(ctx, f)
	out = append(out, l.evidenceAge(req.EvidenceObservedAt)...)
	return append(out, l.concurrency(ctx, req)...)
}

// databaseDowngrades are the signals that do not depend on one request:
// error budget, HA role and the family's safety record.
func (l *Limiter) databaseDowngrades(ctx context.Context, f Family) []Downgrade {
	out := l.budget(ctx)
	out = append(out, l.haState(ctx)...)
	return append(out, l.safety(ctx, f)...)
}

func (l *Limiter) budget(ctx context.Context) []Downgrade {
	if l.b.Budget == nil {
		return nil // no SLO subsystem: no budget that could burn
	}
	st, err := l.b.Budget.ErrorBudget(ctx, l.b.Database)
	switch {
	case err != nil:
		return []Downgrade{{DowngradeBudgetUnavailable, err.Error()}}
	case !st.Configured:
		return nil
	case st.FastBurning:
		return []Downgrade{{DowngradeBudgetBurn, st.Detail}}
	case st.Unknown:
		return []Downgrade{{DowngradeBudgetUnknown, st.Detail}}
	}
	return nil
}

func (l *Limiter) haState(ctx context.Context) []Downgrade {
	if l.b.HA == nil {
		return []Downgrade{{DowngradeHARole, "no HA source"}}
	}
	st, err := l.b.HA.HAStatus(ctx)
	if err != nil {
		return []Downgrade{{DowngradeHARole, err.Error()}}
	}
	var out []Downgrade
	if st.Role != RolePrimary {
		out = append(out, Downgrade{DowngradeHARole, "role " + st.Role})
	}
	if st.SafeMode {
		out = append(out, Downgrade{DowngradeFailover, "HA safe mode (role flapping)"})
	} else if !st.LastRoleChange.IsZero() &&
		l.svc.now().Sub(st.LastRoleChange) < l.svc.cfg.FailoverCooldown {
		out = append(out, Downgrade{DowngradeFailover,
			"role changed at " + st.LastRoleChange.UTC().Format(time.RFC3339)})
	}
	return out
}

func (l *Limiter) evidenceAge(at time.Time) []Downgrade {
	now := l.svc.now()
	switch {
	case at.IsZero():
		return []Downgrade{{DowngradeStaleEvidence, "no evidence time"}}
	case at.After(now.Add(time.Minute)):
		return []Downgrade{{DowngradeStaleEvidence, "evidence time is in the future"}}
	case now.Sub(at) > l.svc.cfg.MaxEvidenceAge:
		return []Downgrade{{DowngradeStaleEvidence,
			fmt.Sprintf("evidence is %s old", now.Sub(at).Round(time.Second))}}
	}
	return nil
}

func (l *Limiter) concurrency(ctx context.Context, req policy.ActionRequest) []Downgrade {
	if l.b.Concurrency == nil || len(req.TargetObjs) == 0 {
		return []Downgrade{{DowngradeConcurrencyUnknown, "no target or no source"}}
	}
	n, err := l.b.Concurrency.ConcurrentActions(ctx, req.TargetObjs, req.LeaseHeld,
		l.svc.cfg.ConcurrencyWindow)
	if err != nil {
		return []Downgrade{{DowngradeConcurrencyUnknown, err.Error()}}
	}
	if n > 0 {
		return []Downgrade{{DowngradeConcurrent,
			fmt.Sprintf("%d other pg_sage actions on the object", n)}}
	}
	return nil
}

func (l *Limiter) safety(ctx context.Context, f Family) []Downgrade {
	n, err := l.svc.store.FamilyViolations(ctx, f, l.svc.now().Add(-l.svc.cfg.SafetyWindow))
	if err != nil {
		return []Downgrade{{DowngradeSafetyRegression, "safety record unreadable: " +
			err.Error()}}
	}
	if n > 0 {
		return []Downgrade{{DowngradeSafetyRegression,
			fmt.Sprintf("%d harmful or unsafe outcomes in the window", n)}}
	}
	return nil
}

// noteTransition logs a cap when its reasons change for this database and
// pair (capped / cap_cleared), not on every authorization.
func (l *Limiter) noteTransition(ctx context.Context, f Family, c ActionClass,
	downs []Downgrade) {
	reasons := make([]string, 0, len(downs))
	for _, d := range downs {
		reasons = append(reasons, d.Reason)
	}
	sort.Strings(reasons)
	key, joined := pairKey{f, c}, strings.Join(reasons, ",")
	l.mu.Lock()
	previous, seen := l.last[key]
	l.mu.Unlock()
	if (seen && previous == joined) || (!seen && joined == "") {
		return
	}
	e := Event{Family: f, Class: c, Type: EventCapped, Actor: ActorPgSage,
		Reason: "downgraded to at most L1: " + joined, Database: l.b.Database,
		At: l.svc.now()}
	if joined == "" {
		e.Type, e.Reason = EventCapCleared, "downgrade signals cleared"
	}
	e.Evidence, _ = json.Marshal(map[string]any{"downgrades": downs})
	if err := l.svc.store.appendEvent(ctx, l.svc.store.pool, e); err != nil {
		l.svc.cfg.logf("autonomy: record %s for %s/%s on %s: %v", e.Type, f, c,
			l.b.Database, err)
		return
	}
	l.mu.Lock()
	l.last[key] = joined
	l.mu.Unlock()
}

// Annotate fills each row's effective level and database-wide downgrades
// for this limiter's database.
func (l *Limiter) Annotate(ctx context.Context, v *View) {
	for i := range v.Families {
		fam := &v.Families[i]
		downs := l.databaseDowngrades(ctx, fam.Family)
		for j := range fam.Classes {
			row := &fam.Classes[j]
			level := MinLevel(row.Granted, row.Cap, row.Supported)
			if row.Provenance == ProvenanceCarriedOver {
				level = MinLevel(row.Granted, carryCap(row.Class))
			}
			if len(downs) > 0 {
				level = MinLevel(level, L1)
				row.Downgrades = downs
			}
			row.Effective = &level
		}
	}
}
