package earned

import (
	"context"
	"fmt"
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
	// ServerMajor is the monitored server's PostgreSQL major (a
	// MajorProbe); nil never caps a level for a major change (earned-07).
	ServerMajor MajorSource
}

// Limiter is the ledger as one database's gate sees it
// (policy.AutonomyLimiter).
type Limiter struct {
	svc *Service
	b   Binding

	mu sync.Mutex
	// noteMu serializes cap and expiry records: last is each pair's
	// recorded cap, warned the expiry last warned about.
	noteMu sync.Mutex
	last   map[pairKey]capState
	warned map[pairKey]time.Time
	// overrides are the mandatory deadline overrides already recorded,
	// by pair, targets and deadline, until the deadline passes.
	overrides map[string]time.Time
}

// Limiter binds the ledger to its database's signals; a binding that
// names another database fails every authorization closed.
func (s *Service) Limiter(b Binding) *Limiter {
	return &Limiter{svc: s, b: b, last: map[pairKey]capState{},
		warned: map[pairKey]time.Time{}, overrides: map[string]time.Time{}}
}

var _ policy.AutonomyLimiter = (*Limiter)(nil)

// Limit is the effective level for req (effectiveLevel) under the
// request's contract and its downgrade signals. Signals are read only for
// a pair granted L2 or more; a ledger read failure is an error (the gate
// then fails closed).
func (l *Limiter) Limit(ctx context.Context, req policy.ActionRequest) (
	policy.AutonomyLimit, error) {
	if err := l.svc.store.checkDatabase(l.b.Database); err != nil {
		return policy.AutonomyLimit{}, fmt.Errorf("limiter bound to another ledger: %w", err)
	}
	f, c := FamilyForRequest(req)
	st, err := l.svc.Granted(ctx, f, c)
	if err != nil {
		return policy.AutonomyLimit{}, err
	}
	out := policy.AutonomyLimit{Granted: int(st.Level), Class: string(c), Family: string(f)}
	var downs []Downgrade
	var major majorCheck
	if st.Level >= L2 {
		downs = l.requestDowngrades(ctx, req, f)
		major = l.majorCap(ctx, st)
	}
	p := &pairEvidence{svc: l.svc, f: f, c: c}
	level, err := effectiveLevel(st, contractCap(req), downs, func() (Level, error) {
		supported, _, err := p.get(ctx)
		return supported, err
	})
	if err != nil {
		return policy.AutonomyLimit{}, err
	}
	if major.note != nil {
		level = MinLevel(level, L2)
	}
	if st.Level >= L2 {
		l.noteLimit(ctx, st, downs, p, major)
	}
	for _, d := range downs {
		out.Downgraded = true
		out.Reasons = append(out.Reasons, d.Reason)
	}
	if major.note != nil {
		out.Reasons = append(out.Reasons, major.note.reason())
	}
	out.Level = int(level)
	return out, nil
}

// effectiveLevel is the one rule for the level the gate allows a pair
// (st's family and class), which the Autonomy and Trust pages show too:
// the granted level, capped by the class (a carried-over level by
// carryCap) and by the contract's reversibility; at most L1 for a class
// that does not remediate the family; at most the level the evidence
// still supports for an earned or grandfathered incident level of L2 or
// more (a carried-over level was granted by policy, not earned by
// evidence, so it does not decay with the evidence; a self-initiated
// level changes only by an approved promotion or a recorded demotion);
// and at most L1 while a downgrade signal holds. supported is read only
// when it can lower the level; its error is returned.
func effectiveLevel(st State, contract Level, downs []Downgrade,
	supported func() (Level, error)) (Level, error) {
	level := MinLevel(st.Level, effectiveCap(st, st.Class), contract)
	if !Applicable(st.Family, st.Class) {
		level = MinLevel(level, L1)
	}
	if level >= L2 && st.Provenance != ProvenanceCarriedOver && !IsSelfInitiated(st.Family) {
		s, err := supported()
		if err != nil {
			return L0, err
		}
		level = MinLevel(level, s)
	}
	if len(downs) > 0 {
		level = MinLevel(level, L1)
	}
	return level, nil
}

// viewLevel is the level the gate would allow a page's row now: the best
// case, under a reversible contract (L3) and the database-wide signals
// only (a request's own evidence age and concurrent actions can lower it).
func viewLevel(st State, supported Level, downs []Downgrade) Level {
	level, err := effectiveLevel(st, L3, downs, func() (Level, error) { return supported, nil })
	if err != nil {
		return L0 // a fixed level cannot fail to read; fail closed regardless
	}
	return level
}

func contractCap(req policy.ActionRequest) Level {
	if req.Contract == nil {
		return L1
	}
	return RollbackCap(req.Contract.RollbackClass)
}

// Governs reports whether the ledger judges req (policy.AutonomyScope):
// an incident-family request or any self-initiated class (roadmap 1.2).
func (l *Limiter) Governs(req policy.ActionRequest) bool { return Governs(req) }

var _ policy.AutonomyScope = (*Limiter)(nil)

// requestDowngrades evaluates every CHECK-40 signal for req. A source
// that cannot answer is itself a downgrade (fail closed). A
// self-initiated request takes the database-wide signals only: it
// carries no incident snapshot (the executor re-reads the finding's
// current evidence before the gate), has no family safety window (its
// demerits demote the class itself), and the executor's cascade cooldown
// and change leases already serialize work on one object (a 15-minute
// cap would withhold, say, the autovacuum tuning that follows a vacuum
// of the same table, which the time ramp let run).
func (l *Limiter) requestDowngrades(ctx context.Context, req policy.ActionRequest,
	f Family) []Downgrade {
	if IsSelfInitiated(f) {
		return l.selfDowngrades(ctx)
	}
	out := l.databaseDowngrades(ctx, f)
	out = append(out, l.evidenceAge(req.EvidenceObservedAt)...)
	return append(out, l.concurrency(ctx, req)...)
}

// selfDowngrades are the database-wide signals of a self-initiated
// class: error budget and HA role.
func (l *Limiter) selfDowngrades(ctx context.Context) []Downgrade {
	return append(l.budget(ctx), l.haState(ctx)...)
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
	return safetyDowngrades(l.svc.store.FamilyViolations(ctx, f,
		l.svc.now().Add(-l.svc.cfg.SafetyWindow)))
}

// safetyDowngrades is the signal of a family's harmful or unsafe outcomes
// in the window (n), or of its unreadable record.
func safetyDowngrades(n int, err error) []Downgrade {
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

// Annotate fills each row's effective level and database-wide downgrades
// for this limiter's database: the error budget and HA role read once,
// the families' safety record reused from the view (read in one
// statement when the view carries none).
func (l *Limiter) Annotate(ctx context.Context, v *View) {
	if len(v.Families) == 0 {
		return
	}
	families := make([]string, 0, len(v.Families))
	for _, fam := range v.Families {
		families = append(families, string(fam.Family))
	}
	sig := l.viewSignals(ctx, v.safety, families)
	mv := l.majorView(ctx)
	for i := range v.Families {
		fam := &v.Families[i]
		downs := sig.of(fam.Family)
		for j := range fam.Classes {
			row := &fam.Classes[j]
			st := State{Family: fam.Family, Class: row.Class, Level: row.Granted,
				Provenance: row.Provenance, ServerMajor: row.ServerMajor}
			level := viewLevel(st, row.Supported, downs)
			if row.MajorCap = mv.capOf(ctx, st); row.MajorCap != nil {
				level = MinLevel(level, L2)
			}
			if len(downs) > 0 {
				row.Downgrades = downs
			}
			row.Effective = &level
		}
	}
}

// viewSignals are the database-wide downgrade signals a page shows.
type viewSignals struct {
	self     []Downgrade
	safety   map[Family]familySafetyRow
	err      error
	byFamily map[Family][]Downgrade
}

// viewSignals reads the error budget and HA role once, and the incident
// families' safety record unless safety already holds it.
func (l *Limiter) viewSignals(ctx context.Context, safety map[Family]familySafetyRow,
	families []string) *viewSignals {
	sig := &viewSignals{self: l.selfDowngrades(ctx), safety: safety,
		byFamily: map[Family][]Downgrade{}}
	if sig.safety == nil {
		sig.safety, sig.err = l.svc.store.safetySet(ctx, families,
			l.svc.now().Add(-l.svc.cfg.SafetyWindow))
	}
	return sig
}

// of is a family's signals: the database-wide ones, and for an incident
// family its safety record.
func (s *viewSignals) of(f Family) []Downgrade {
	if IsSelfInitiated(f) {
		return s.self
	}
	if downs, ok := s.byFamily[f]; ok {
		return downs
	}
	downs := append(append([]Downgrade(nil), s.self...),
		safetyDowngrades(s.safety[f].n, s.err)...)
	s.byFamily[f] = downs
	return downs
}
