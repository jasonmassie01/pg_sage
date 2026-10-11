package agentguard

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// KillDeps wires a Switch. Store is the control database (a nil or
// pool-less store is an unavailable control database: the kill still
// runs per database); Targets lists the monitored databases at call time.
type KillDeps struct {
	Store    *Store
	Keyring  *crypto.Keyring
	Targets  func(context.Context) ([]KillTarget, error)
	Memory   []MemoryRevoker
	Fallback *FallbackLog
	Config   KillConfig
}

// Switch is the kill switch, freeze and unfreeze.
type Switch struct {
	store    *Store
	keyring  *crypto.Keyring
	targets  func(context.Context) ([]KillTarget, error)
	memory   []MemoryRevoker
	fallback *FallbackLog
	cfg      KillConfig
}

// NewSwitch validates the configuration.
func NewSwitch(d KillDeps) (*Switch, error) {
	if d.Targets == nil {
		return nil, invalid("the kill switch needs its databases")
	}
	if err := d.Config.Validate(); err != nil {
		return nil, err
	}
	store := d.Store
	if store == nil {
		store = NewStore(nil)
	}
	return &Switch{store: store, keyring: d.Keyring, targets: d.Targets, memory: d.Memory,
		fallback: d.Fallback, cfg: d.Config}, nil
}

// Kill runs the kill switch (§6.10 steps 1-8). It returns an error only
// for a request it cannot start (invalid, unknown principal or database,
// no databases); every step failure after that is in the report, and the
// remaining steps still run.
func (s *Switch) Kill(ctx context.Context, req KillRequest) (KillReport, error) {
	if err := req.Validate(); err != nil {
		return KillReport{}, err
	}
	k := killScope{actionType: "guard_kill", scope: req.Scope, all: req.Scope == KillScopeAll,
		reason: req.Reason, actor: req.Actor}
	if req.Scope == KillScopePrincipal {
		k.ids = []string{req.ID}
	}
	if req.Scope == KillScopeDatabase {
		k.database = req.ID
	}
	return s.contain(ctx, k, true)
}

// Freeze freezes one principal on an operator's word: as a principal
// kill, but its tokens stay (they authenticate to agent_frozen) and its
// unfreeze needs one admin.
func (s *Switch) Freeze(ctx context.Context, req FreezeRequest) (KillReport, error) {
	if err := req.Validate(); err != nil {
		return KillReport{}, err
	}
	k := killScope{actionType: "guard_freeze", scope: KillScopePrincipal,
		ids: []string{req.PrincipalID}, reason: req.Reason, actor: req.Actor}
	return s.contain(ctx, k, false)
}

// contain is the shared pipeline of Kill and Freeze.
func (s *Switch) contain(ctx context.Context, k killScope, kill bool) (KillReport, error) {
	targets, err := s.scopeTargets(ctx, k)
	if err != nil {
		return KillReport{}, err
	}
	rep := KillReport{Scope: k.scope, ID: k.database, Reason: k.reason,
		StartedAt: time.Now().UTC(), Principals: append([]string{}, k.ids...)}
	if k.scope == KillScopePrincipal {
		rep.ID = k.ids[0]
	}
	if kill {
		k.killID, err = s.startKill(ctx, KillRequest{Scope: k.scope, ID: rep.ID,
			Reason: k.reason, Actor: k.actor})
		rep.KillID = k.killID
		rep.ControlError = joinErr(rep.ControlError, err)
	}
	s.revokeMemory(ctx, k)
	mark := s.freezeMark(k)
	ids, freezeErr := s.markFrozen(ctx, mark)
	if freezeErr == nil && k.all {
		k.ids = ids
	}
	if kill && freezeErr == nil {
		rep.TokensRevoked, err = s.revokeTokens(ctx, k.ids, k.actor)
		rep.ControlError = joinErr(rep.ControlError, err)
	}
	runs := s.runClusters(ctx, k, targets)
	if freezeErr != nil {
		freezeErr = s.retryFreeze(ctx, mark, freezeErr)
	}
	if freezeErr != nil {
		rep.ControlError = joinErr(rep.ControlError, freezeErr)
		s.auditControlFailure(k, freezeErr)
	}
	s.verify(ctx, runs)
	return s.finishReport(ctx, rep, k, runs), nil
}

// scopeTargets checks the scope exists and returns the databases it
// covers.
func (s *Switch) scopeTargets(ctx context.Context, k killScope) ([]KillTarget, error) {
	if k.scope == KillScopePrincipal {
		// Only an unknown principal stops the kill. With the control
		// database down the role names still follow from the id, and a
		// retired principal's roles may outlive it by the grace period.
		if _, err := s.store.Get(ctx, k.ids[0]); isNotFound(err) {
			return nil, err
		}
	}
	targets, err := s.targets(ctx)
	if err != nil {
		return nil, fmt.Errorf("agentguard: listing the databases to kill: %w", err)
	}
	if k.scope != KillScopeDatabase {
		if len(targets) == 0 {
			return nil, fmt.Errorf("%w: no monitored database to reach", ErrUnavailable)
		}
		return targets, nil
	}
	for _, t := range targets {
		if t.Name == k.database {
			return []KillTarget{t}, nil
		}
	}
	return nil, fmt.Errorf("%w: database %q", ErrNotFound, k.database)
}

func (s *Switch) freezeMark(k killScope) freezeMark {
	m := freezeMark{all: k.all, ids: k.ids, actor: k.actor, killID: k.killID,
		reason: "freeze: " + k.reason}
	if k.actionType == "guard_kill" {
		m.reason = "kill: " + k.reason
	}
	switch k.scope {
	case KillScopeAll:
		m.flagScope = "fleet"
	case KillScopeDatabase:
		m.flagScope, m.flagTarget = "database", k.database
	}
	return m
}

// retryFreeze retries a freeze that timed out on a lock, after the
// clusters' steps cancelled the in-flight applies holding it.
func (s *Switch) retryFreeze(ctx context.Context, m freezeMark, err error) error {
	for attempt := 1; attempt < s.cfg.Attempts && isLockTimeout(err); attempt++ {
		_, err = s.markFrozen(ctx, m)
	}
	if isLockTimeout(err) {
		return fmt.Errorf("agentguard: could not lock the principals' rows within %d "+
			"attempts (an agent apply holds them): %w", s.cfg.Attempts, err)
	}
	return err
}

func (s *Switch) revokeMemory(ctx context.Context, k killScope) {
	if k.scope == KillScopeDatabase {
		return
	}
	for _, m := range s.memory {
		m.RevokePrincipals(ctx, append([]string(nil), k.ids...), k.all)
	}
}

// auditControlFailure queues the unrecorded freeze in the local log.
func (s *Switch) auditControlFailure(k killScope, err error) {
	e := FallbackEntry{ActionType: k.actionType, Database: "(control)",
		Scope: string(k.scope), Target: k.database, Reason: k.reason, Actor: k.actor,
		Outcome: "failed", Error: err.Error()}
	if len(k.ids) == 1 && !k.all {
		e.PrincipalID = k.ids[0]
	}
	_ = s.fallback.Append(e) // no log configured: the report carries the failure
}

// runClusters runs every cluster's steps in parallel.
func (s *Switch) runClusters(ctx context.Context, k killScope,
	targets []KillTarget) []*clusterRun {
	inflight := s.inflightByDatabase(ctx, k)
	var runs []*clusterRun
	for _, group := range groupByCluster(targets) {
		r := newClusterRun(s, k, group[0].ClusterKey, group)
		r.inflight = inflight
		r.registered = s.registeredRoles(ctx, k, r.key)
		runs = append(runs, r)
	}
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func(r *clusterRun) {
			defer wg.Done()
			r.run(ctx)
		}(r)
	}
	wg.Wait()
	return runs
}

func (s *Switch) inflightByDatabase(ctx context.Context, k killScope) map[string][]int {
	out := map[string][]int{}
	rows, err := s.inflight(ctx, k.ids, k.anyRole())
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.databaseID] = append(out[r.databaseID], r.pid)
	}
	return out
}

// registeredRoles are the scope's principals' registered roles on a
// cluster; none when the control database is down.
func (s *Switch) registeredRoles(ctx context.Context, k killScope, key string) []ClusterRole {
	if !k.disablesRoles() {
		return nil
	}
	if k.all {
		rs, _ := s.store.ClusterRolesOn(ctx, key)
		return rs
	}
	var out []ClusterRole
	for _, id := range k.ids {
		if cr, err := s.store.ClusterRolesOf(ctx, id, key); err == nil && cr != nil {
			out = append(out, *cr)
		}
	}
	return out
}

// groupByCluster groups targets by cluster key, keeping their order; a
// target without a key is its own cluster.
func groupByCluster(targets []KillTarget) [][]KillTarget {
	var out [][]KillTarget
	index := map[string]int{}
	for _, t := range targets {
		if i, ok := index[t.ClusterKey]; ok && t.ClusterKey != "" {
			out[i] = append(out[i], t)
			continue
		}
		index[t.ClusterKey] = len(out)
		out = append(out, []KillTarget{t})
	}
	return out
}

// finishReport sums the runs and stores the kill's report.
func (s *Switch) finishReport(ctx context.Context, rep KillReport, k killScope,
	runs []*clusterRun) KillReport {
	rep.Principals = append([]string{}, k.ids...)
	rep.Databases = []DatabaseReport{}
	rep.Verified = true
	for _, r := range runs {
		for _, d := range r.dbs {
			rep.Databases = append(rep.Databases, d)
			rep.ApprovalsCancelled += d.ApprovalsCancelled
			rep.Verified = rep.Verified && d.Verified
		}
	}
	rep.FinishedAt = time.Now().UTC()
	if err := s.finishKill(ctx, rep); err != nil {
		rep.ControlError = joinErr(rep.ControlError, err)
	}
	return rep
}
