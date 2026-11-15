package analyzer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// OutOfCycle is one producer run between analyzer cycles, such as the
// agent posture exposure sweep (fleetpol-02a). Its findings go through
// the same upsert, notification, suppression and resolution path as a
// cycle's, for its own categories only.
type OutOfCycle struct {
	Findings []Finding
	// Evaluated are the categories the run fully evaluated: their open
	// findings that the run did not report again resolve.
	Evaluated []string
	// Incomplete are categories the run did not fully evaluate (a failed
	// or truncated read): their open findings stay open.
	Incomplete []string
}

// OutOfCycleRun runs a producer once. ran is false when it had nothing to
// do (its input did not change); nothing is published then.
type OutOfCycleRun func(ctx context.Context) (run OutOfCycle, ran bool, err error)

// Out-of-cycle errors, each distinguishable with errors.Is.
var (
	ErrNoOutOfCycleRun  = errors.New("analyzer: no out-of-cycle run")
	ErrOutOfCycleNoPool = errors.New("analyzer: out-of-cycle findings have no database")
)

// RunOutOfCycle runs fn while no cycle runs, then publishes what it
// found. It waits for a running cycle to finish (or ctx to end), so a
// run never interleaves with a cycle's own read and resolution of the
// same categories. It returns fn's error, or the publish error.
func (a *Analyzer) RunOutOfCycle(ctx context.Context, fn OutOfCycleRun) error {
	if fn == nil {
		return ErrNoOutOfCycleRun
	}
	if err := a.cycleMu.lockCtx(ctx); err != nil {
		return fmt.Errorf("analyzer: wait for the running cycle: %w", err)
	}
	defer a.cycleMu.Unlock()
	if a.collector != nil && a.collector.LatestSnapshot() == nil {
		// Its findings are derived on the latest snapshot; the first
		// cycle, which waits for that snapshot, runs every detector.
		a.logFn("DEBUG", "analyzer: out-of-cycle run skipped: no collector snapshot")
		return nil
	}
	run, ran, err := fn(ctx)
	if err != nil {
		return fmt.Errorf("analyzer: out-of-cycle run: %w", err)
	}
	if !ran {
		return nil
	}
	return a.publishOutOfCycle(ctx, run)
}

// publishOutOfCycle persists and resolves run as a cycle would: under a
// cycle evaluation of its own, so the rule pack's disabled categories are
// dropped and the evidence time is now, and for the run's own categories
// only. The cycle's evaluation is restored afterwards.
func (a *Analyzer) publishOutOfCycle(ctx context.Context, run OutOfCycle) error {
	if a.pool == nil {
		return ErrOutOfCycleNoPool
	}
	prev := a.eval
	a.eval = newCycleEval()
	defer func() { a.eval = prev }()
	a.eval.collectedAt = time.Now()
	own := outOfCycleCategories(run, run.Findings)
	findings := a.deriveOutOfCycle(ctx, run.Findings, own)
	a.eval.evaluated(run.Evaluated...)
	a.eval.fail(run.Incomplete...)
	resolvable := onlyCategories(a.eval.resolvable(findings), own)
	published, upsertErr, err := a.persistFindings(ctx, findings)
	if err != nil {
		return fmt.Errorf("analyzer: out-of-cycle: filter suppressed findings: %w", err)
	}
	a.mergeOutOfCycle(published, own)
	a.recordRecommendations(ctx, published, resolvable)
	a.resolveCleared(ctx, published, resolvable)
	a.logFn("INFO", "analyzer: out-of-cycle run: %d findings in %d categories",
		len(published), len(resolvable))
	return upsertErr
}

// deriveOutOfCycle rewrites findings as a cycle does (deriveFindings on
// the latest snapshot: app-managed indexes, confirmed facts, clone-schema
// families, the rule pack) and keeps those of the run's own categories. A
// finding a cycle folds into a derived category is then not reported by
// the run either, so the next full cycle does not resolve it, and derived
// categories stay the cycle's to report and resolve.
func (a *Analyzer) deriveOutOfCycle(ctx context.Context, findings []Finding,
	own map[string]bool) []Finding {
	if a.collector == nil {
		findings = a.applyDisabledRules(findings)
	} else {
		current := snapshotForAnalysis(a.collector.LatestSnapshot())
		filterSchemaExclusions(current)
		excluded := a.excludeFactSchemas(ctx, current, nil)
		findings = a.deriveFindings(ctx, current, findings, excluded)
	}
	out := findings[:0:0]
	for _, f := range findings {
		if own[f.Category] {
			out = append(out, f)
		}
	}
	return out
}

// onlyCategories is the categories of set that are in own.
func onlyCategories(set, own map[string]bool) map[string]bool {
	out := make(map[string]bool, len(set))
	for c := range set {
		if own[c] {
			out[c] = true
		}
	}
	return out
}

// outOfCycleCategories are the run's own categories, whose latest
// findings it replaces: those it evaluated or reported.
func outOfCycleCategories(run OutOfCycle, findings []Finding) map[string]bool {
	out := make(map[string]bool, len(run.Evaluated)+len(findings))
	for _, c := range run.Evaluated {
		out[c] = true
	}
	for _, f := range findings {
		out[f.Category] = true
	}
	return out
}

// mergeOutOfCycle replaces the latest findings of categories with
// published and keeps every other category's.
func (a *Analyzer) mergeOutOfCycle(published []Finding, categories map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	merged := make([]Finding, 0, len(a.findings)+len(published))
	for _, f := range a.findings {
		if !categories[f.Category] {
			merged = append(merged, f)
		}
	}
	a.findings = append(merged, published...)
}

// cycleLock serializes cycles with out-of-cycle runs. Unlike a mutex, a
// wait for it can be abandoned when the waiter's context ends. The zero
// value is unlocked.
type cycleLock struct {
	once sync.Once
	ch   chan struct{}
}

func (l *cycleLock) sem() chan struct{} {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	return l.ch
}

// Lock waits for the lock.
func (l *cycleLock) Lock() { l.sem() <- struct{}{} }

// Unlock releases the lock.
func (l *cycleLock) Unlock() { <-l.sem() }

// lockCtx waits for the lock until ctx ends.
func (l *cycleLock) lockCtx(ctx context.Context) error {
	select {
	case l.sem() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
