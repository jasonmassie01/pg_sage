package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/policy"
)

const (
	// policyCeilingTTL bounds how stale a policy token cap may be.
	policyCeilingTTL = 60 * time.Second
	// policyCeilingReadTimeout bounds the control-DB read on admission.
	policyCeilingReadTimeout = 2 * time.Second
)

// policyCeiling turns the standing policy's budgets.llm_tokens_daily into
// an LLM client ceiling (llm-03). It reads at most once per TTL; a read
// error keeps the last value and warns. No policy, or a NoCap limit,
// means no ceiling (config only).
type policyCeiling struct {
	label string
	read  func(context.Context) (policy.Policy, error)
	now   func() time.Time
	warn  func(msg string, args ...any)

	mu      sync.Mutex
	fetched time.Time
	loaded  bool
	value   int64
	present bool
}

func newPolicyCeiling(
	store *policy.Store, scope policy.Scope, label string,
) *policyCeiling {
	return &policyCeiling{
		label: label,
		read: func(ctx context.Context) (policy.Policy, error) {
			return store.Current(ctx, scope)
		},
		now: time.Now,
		warn: func(msg string, args ...any) {
			logWarn("llm", msg, args...)
		},
	}
}

// Ceiling returns the policy cap, refreshing it when the cache expired.
// Concurrent callers wait for one read.
func (p *policyCeiling) Ceiling() (int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.loaded && now.Sub(p.fetched) < policyCeilingTTL {
		return p.value, p.present
	}
	p.fetched, p.loaded = now, true
	value, present, err := p.fetch()
	if err != nil {
		p.warn("db %q: reading the policy LLM token cap failed; keeping the "+
			"last value (%s): %v", p.label, p.describe(), err)
		return p.value, p.present
	}
	p.value, p.present = value, present
	return value, present
}

func (p *policyCeiling) fetch() (int64, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), policyCeilingReadTimeout)
	defer cancel()
	current, err := p.read(ctx)
	if errors.Is(err, policy.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	doc, err := policy.ParseDocument(current.Document)
	if err != nil {
		return 0, false, err
	}
	limit := doc.Budgets.LLMTokensDaily
	value, ok := limit.Value()
	if !ok {
		return 0, false, nil
	}
	return value, true, nil
}

func (p *policyCeiling) describe() string {
	if !p.present {
		return "no policy cap"
	}
	return formatTokensPerDay(p.value)
}

// policyStoreScope is the standing policy this runtime enforces: the
// database's scope in meta-DB mode, the global one otherwise.
func (rt *databaseRuntime) policyStoreScope() (*policy.Store, policy.Scope) {
	pool := rt.spec.ControlPool
	if pool == nil {
		pool = rt.spec.Pool
	}
	var scope policy.Scope
	if rt.spec.DatabaseID > 0 {
		id := int64(rt.spec.DatabaseID)
		scope.DatabaseID = &id
	}
	return policy.NewStore(pool), scope
}

// attachPolicyCeiling caps the clients by the policy's llm_tokens_daily.
// Without a control pool there is no policy to read.
func (rt *databaseRuntime) attachPolicyCeiling(clients ...*llm.Client) {
	if rt.spec.ControlPool == nil && rt.spec.Pool == nil {
		return
	}
	store, scope := rt.policyStoreScope()
	ceiling := newPolicyCeiling(store, scope, rt.spec.Name)
	for _, client := range clients {
		if client != nil {
			client.SetCeiling(ceiling.Ceiling)
		}
	}
}
