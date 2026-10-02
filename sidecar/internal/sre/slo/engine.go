package slo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// ScopeFunc resolves the database scope (the investigator's binding).
type ScopeFunc func(ctx context.Context) (sre.Scope, error)

// PageHandler is told about a page-level burn once per burn (and again
// after it fails). The wiring opens an slo_burn investigation.
type PageHandler func(ctx context.Context, st Status) error

// Engine errors; callers distinguish them with errors.Is.
var (
	ErrUnknownSLO = errors.New("unknown SLO")
	ErrNotPush    = errors.New("SLO does not accept pushed samples")
)

// EngineDeps wires one database's SLO engine.
type EngineDeps struct {
	// Database is the display name the statuses carry.
	Database string
	// Objectives are the app SLOs of this database.
	Objectives []Objective
	// Proxies are the database proxy SLIs.
	Proxies []Proxy
	// Rules are the burn-rate rules; empty means DefaultRules.
	Rules      []Rule
	Store      *Store
	Prometheus *PromClient
	Scope      ScopeFunc
	OnPage     PageHandler
	Interval   time.Duration
	Logf       func(level, msg string, args ...any)
	Now        func() time.Time
}

// Engine evaluates one database's SLOs every interval, persists their
// error-budget state and reports page-level burns.
type Engine struct {
	database   string
	objectives []Objective
	proxies    map[string]Proxy
	rules      []Rule
	store      *Store
	prom       PromSource
	scope      ScopeFunc
	onPage     PageHandler
	interval   time.Duration
	logf       func(level, msg string, args ...any)
	now        func() time.Time

	evalMu    sync.Mutex
	mu        sync.Mutex
	totals    map[string]PushSample
	reasons   map[string]string
	paged     map[string]time.Time
	pruned    bool
	lastPurge time.Time
}

// NewEngine validates the objectives (unique names, valid definitions)
// and the rules.
func NewEngine(d EngineDeps) (*Engine, error) {
	if d.Store == nil || d.Scope == nil || d.Interval <= 0 {
		return nil, fmt.Errorf("%w: SLO engine needs a store, a scope and an interval",
			sre.ErrInvalidRequest)
	}
	rules := d.Rules
	if len(rules) == 0 {
		rules = DefaultRules()
	}
	if err := ValidateRules(rules); err != nil {
		return nil, err
	}
	e := &Engine{database: d.Database, proxies: map[string]Proxy{}, rules: rules,
		store: d.Store, prom: PromSource{Client: d.Prometheus}, scope: d.Scope,
		onPage: d.OnPage, interval: d.Interval, logf: d.Logf, now: d.Now,
		totals: map[string]PushSample{}, reasons: map[string]string{},
		paged: map[string]time.Time{}}
	if e.logf == nil {
		e.logf = func(string, string, ...any) {}
	}
	if e.now == nil {
		e.now = time.Now
	}
	objs := append([]Objective(nil), d.Objectives...)
	for _, p := range d.Proxies {
		o := p.Objective()
		e.proxies[o.Name] = p
		objs = append(objs, o)
	}
	return e, e.setObjectives(objs)
}

func (e *Engine) setObjectives(objs []Objective) error {
	seen := map[string]bool{}
	for i := range objs {
		objs[i].Database = e.database
		if err := objs[i].Validate(); err != nil {
			return err
		}
		if seen[objs[i].Name] {
			return fmt.Errorf("%w: duplicate SLO name %q", ErrInvalidObjective, objs[i].Name)
		}
		seen[objs[i].Name] = true
	}
	e.objectives = objs
	return nil
}

// Database is the display name of the engine's database.
func (e *Engine) Database() string { return e.database }

// Rules are the burn-rate rules in use.
func (e *Engine) Rules() []Rule { return append([]Rule(nil), e.rules...) }

// Objectives are the SLOs the engine evaluates.
func (e *Engine) Objectives() []Objective { return append([]Objective(nil), e.objectives...) }

func (e *Engine) objective(name string) (Objective, bool) {
	for _, o := range e.objectives {
		if o.Name == name {
			return o, true
		}
	}
	return Objective{}, false
}

// Run evaluates every interval until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		if _, err := e.EvaluateOnce(ctx); err != nil && ctx.Err() == nil {
			e.logf("WARN", "sre slo: evaluating the SLOs of %s failed: %v", e.database, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// EvaluateOnce samples the proxies, evaluates and persists every SLO and
// reports page-level burns. One SLO's failure does not stop the others.
func (e *Engine) EvaluateOnce(ctx context.Context) ([]Status, error) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	scope, err := e.scope(ctx)
	if err != nil {
		return nil, err
	}
	// Microseconds, as stored: a burn's start read back equals the one
	// reported.
	now := e.now().UTC().Truncate(time.Microsecond)
	var errs []error
	if err := e.prune(ctx, scope); err != nil {
		errs = append(errs, err)
	}
	e.sampleProxies(ctx, scope, now)
	out := make([]Status, 0, len(e.objectives))
	for _, o := range e.objectives {
		st := Evaluate(o, e.rules, e.windows(ctx, scope, o, now),
			e.window(ctx, scope, o, o.Window, now), now)
		saved, _, err := e.store.Save(ctx, scope, st)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, saved)
		e.maybePage(ctx, saved)
	}
	e.purge(ctx, scope, now)
	return out, errors.Join(errs...)
}

func (e *Engine) prune(ctx context.Context, scope sre.Scope) error {
	if e.pruned {
		return nil
	}
	names := make([]string, 0, len(e.objectives))
	for _, o := range e.objectives {
		names = append(names, o.Name)
	}
	if err := e.store.Prune(ctx, scope, names); err != nil {
		return err
	}
	e.pruned = true
	return nil
}

// windows evaluates every distinct window of the rules.
func (e *Engine) windows(ctx context.Context, scope sre.Scope, o Objective,
	now time.Time) map[time.Duration]Window {
	out := map[time.Duration]Window{}
	for _, r := range e.rules {
		for _, d := range []time.Duration{r.Long, r.Short} {
			if _, done := out[d]; !done {
				out[d] = e.window(ctx, scope, o, d, now)
			}
		}
	}
	return out
}

// window reads one window from the objective's source.
func (e *Engine) window(ctx context.Context, scope sre.Scope, o Objective, d time.Duration,
	now time.Time) Window {
	if o.Source == SourcePrometheus {
		return e.prom.Window(ctx, o, d, now)
	}
	series := ""
	if o.Source == SourceProxy {
		series = string(scope.DatabaseID)
	}
	from := now.Add(-d)
	aggs, err := e.store.Aggregate(ctx, scope.DeploymentID, o.Name, series, from, now,
		o.staleAfter())
	if err != nil {
		e.logf("WARN", "sre slo: reading %s of %s failed: %v", FormatWindow(d), o.Name, err)
		return Window{Duration: d, Unknown: ReasonSourceError}
	}
	w := Combine(aggs, o, from, now, now)
	if reason := e.proxyReason(o.Name); reason != "" && w.Unknown != "" && w.Eligible == 0 {
		w.Unknown = reason
	}
	return w
}

func (e *Engine) proxyReason(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reasons[name]
}

// maybePage reports a page-level burn once per burn; a failed report is
// retried on the next evaluation.
func (e *Engine) maybePage(ctx context.Context, st Status) {
	if e.onPage == nil {
		return
	}
	e.mu.Lock()
	if st.State != StatePage || st.BurnStartedAt == nil {
		delete(e.paged, st.Name)
		e.mu.Unlock()
		return
	}
	done := e.paged[st.Name].Equal(*st.BurnStartedAt)
	e.mu.Unlock()
	if done {
		return
	}
	if err := e.onPage(ctx, st); err != nil {
		e.logf("WARN", "sre slo: reporting the page-level burn of %s failed (retried): %v",
			st.Name, err)
		return
	}
	e.mu.Lock()
	e.paged[st.Name] = *st.BurnStartedAt
	e.mu.Unlock()
}

// Retention.
const (
	purgeInterval        = time.Hour
	transitionsRetention = 90 * 24 * time.Hour
)

func (e *Engine) purge(ctx context.Context, scope sre.Scope, now time.Time) {
	if now.Sub(e.lastPurge) < purgeInterval {
		return
	}
	longestRule := time.Duration(0)
	for _, r := range e.rules {
		longestRule = max(longestRule, r.Long)
	}
	var total int64
	for _, o := range e.objectives {
		keep := max(o.Window, longestRule) + 24*time.Hour
		n, err := e.store.Purge(ctx, scope, o.Name, now.Add(-keep),
			now.Add(-transitionsRetention))
		if err != nil {
			e.logf("WARN", "sre slo: retention of %s failed: %v", o.Name, err)
			return
		}
		total += n
	}
	if total > 0 {
		e.logf("INFO", "sre slo: retention deleted %d samples and transitions", total)
	}
	e.lastPurge = now
}
