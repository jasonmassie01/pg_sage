package selfconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// ErrOperatorSetUnknown: a pass that cannot tell which keys the operator
// set must not derive anything (it could overwrite an operator's value).
var ErrOperatorSetUnknown = errors.New("selfconfig: the operator-set keys are unknown")

// Engine runs derivation passes for one database.
type Engine struct {
	Store *Store
	Rules []Rule
	Now   func() time.Time
}

// NewEngine returns an engine over the registered rules.
func NewEngine(store *Store) *Engine {
	return &Engine{Store: store, Rules: Rules(), Now: time.Now}
}

// Input is one pass.
type Input struct {
	// Cfg is the database's runtime configuration; in-force derived values
	// are written into it.
	Cfg *config.Config
	// Operator is the configuration as the operator declared it (file,
	// overrides), for operator-set values; nil means Cfg.
	Operator *config.Config
	// OperatorSet names the keys the operator set; nil fails closed.
	OperatorSet map[string]bool
	Evidence    Evidence
	Phase       Phase
}

// Result is one key's outcome of a pass.
type Result struct {
	Key     string
	Status  Status
	Value   float64
	Pending *float64
	Shadow  *float64
	Note    string
	Applied bool
	Events  []Event
}

type keyStep struct {
	rule    Rule
	prev    State
	running float64
	res     StepResult
}

func (in Input) validate() error {
	switch {
	case in.Cfg == nil:
		return errors.New("selfconfig: no runtime configuration")
	case in.OperatorSet == nil:
		return ErrOperatorSetUnknown
	case in.Phase != PhaseStartup && in.Phase != PhaseLive:
		return fmt.Errorf("selfconfig: unknown phase %q", in.Phase)
	}
	return nil
}

// Reconcile runs one pass: every rule is stepped under one lock, the
// states and ledger entries are committed, then the in-force values are
// written into the runtime config. Nothing is written when the commit
// fails.
func (e *Engine) Reconcile(ctx context.Context, in Input) ([]Result, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	if err := ValidateRules(e.Rules); err != nil {
		return nil, err
	}
	tx, err := e.Store.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	states, err := loadStates(ctx, tx, "")
	if err != nil {
		return nil, err
	}
	now := e.Now()
	steps := e.step(in, states, now)
	guardValidity(in.Cfg, steps, now)
	for _, s := range steps {
		if err := saveState(ctx, tx, s.res.Next); err != nil {
			return nil, err
		}
		if err := appendEvents(ctx, tx, s.res.Next, s.res.Events, "pg_sage", now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("selfconfig: commit the derivation pass: %w", err)
	}
	return applySteps(in.Cfg, steps), nil
}

func (e *Engine) step(in Input, states map[string]State, now time.Time) []keyStep {
	operator := in.Operator
	if operator == nil {
		operator = in.Cfg
	}
	soak := in.Cfg.SelfConfig.Soak()
	if soak <= 0 {
		soak = config.DefaultConfig().SelfConfig.Soak()
	}
	steps := make([]keyStep, 0, len(e.Rules))
	for _, r := range e.Rules {
		si := StepInput{Rule: r, Restart: isRestart(r.Key), Phase: in.Phase, Now: now,
			Soak: soak, Prev: states[r.Key], Proposal: r.Evaluate(in.Evidence, in.Cfg),
			// Without a promoted value the runtime keeps the value its own
			// configuration gave it: derivation never writes a default over it.
			Running: r.Get(in.Cfg), Default: r.Get(in.Cfg), OperatorSet: in.OperatorSet[r.Key],
			Operator: r.Get(operator), Cfg: in.Cfg}
		if r.Observe != nil {
			if v, ok := r.Observe(in.Evidence, in.Cfg); ok {
				si.Sample = &v
			}
		}
		steps = append(steps, keyStep{rule: r, prev: si.Prev, running: si.Running,
			res: Step(si)})
	}
	return steps
}

func isRestart(key string) bool {
	f, ok := config.LookupFieldLifecycle(key)
	return !ok || f.Lifecycle == config.LifecycleRestart
}

// guardValidity holds every derived value that would make a valid runtime
// configuration invalid: it is kept in shadow with the reason, never
// applied.
func guardValidity(cfg *config.Config, steps []keyStep, now time.Time) {
	if cfg.Validate() != nil {
		return // already invalid for other reasons: nothing to judge against
	}
	all := config.Clone(cfg)
	for _, s := range steps {
		if s.res.Apply {
			s.rule.Set(all, s.res.Next.Value)
		}
	}
	if all.Validate() == nil {
		return
	}
	for i := range steps {
		s := &steps[i]
		if !s.res.Apply {
			continue
		}
		one := config.Clone(cfg)
		s.rule.Set(one, s.res.Next.Value)
		if err := one.Validate(); err != nil {
			s.res = holdInvalid(*s, err, now)
		}
	}
}

func holdInvalid(s keyStep, err error, now time.Time) StepResult {
	attempted := s.res.Next.Value
	next := copyState(s.prev)
	next.Key, next.Rule, next.RuleVersion = s.rule.Key, s.rule.Name, s.rule.Version
	next.Evidence, next.Bounds = s.res.Next.Evidence, s.res.Next.Bounds
	next.Value, next.Pending = s.running, nil
	if next.Shadow == nil || !equal(*next.Shadow, attempted) {
		next.Shadow, next.ShadowSince, next.Samples = ptr(attempted), now, nil
	}
	reason := "would make the configuration invalid: " + err.Error()
	var events []Event
	if next.ShadowReason != reason {
		events = []Event{{Kind: EventHeld, Value: ptr(attempted), Previous: ptr(s.running),
			Reason: reason}}
	}
	next.ShadowOutcome, next.ShadowReason = OutcomeWorse, reason
	next.Status, next.UpdatedAt = next.status(), now
	return StepResult{Next: next, Events: events}
}

func applySteps(cfg *config.Config, steps []keyStep) []Result {
	out := make([]Result, 0, len(steps))
	config.LockForHotReload()
	defer config.UnlockForHotReload()
	for _, s := range steps {
		n := s.res.Next
		if s.res.Apply {
			s.rule.Set(cfg, n.Value)
		}
		out = append(out, Result{Key: n.Key, Status: n.Status, Value: n.Value,
			Pending: n.Pending, Shadow: n.Shadow, Note: n.Note, Applied: s.res.Apply,
			Events: s.res.Events})
	}
	return out
}

// Summary is the one-line startup report of a pass.
func Summary(results []Result) string {
	if len(results) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(results))
	for _, r := range results {
		detail := string(r.Status)
		if r.Shadow != nil {
			detail = fmt.Sprintf("shadow %g", *r.Shadow)
		}
		if r.Pending != nil {
			detail += fmt.Sprintf(", %g pending restart", *r.Pending)
		}
		parts = append(parts, fmt.Sprintf("%s=%g (%s)", r.Key, r.Value, detail))
	}
	return strings.Join(parts, "; ")
}
