// Package tuning is pg_sage's case-driven tuning agent (roadmap 2.2): one
// agent per database replaces the optimizer, advisor and tuner prompts.
// Each analyzer cycle it classifies the workload deterministically,
// detects the cases worth tuning (top statements, regressions, write
// amplification), asks the model about each case with read-only tools
// within a per-database budget, validates every typed proposal with the
// same gates as before (optimizer admission and HypoPG, configuration
// allowlists, the tuner's hint checks, confirmed facts, operator
// rejections), and ranks the admitted proposals by a confidence
// calibrated on the outcome ledger. Its findings run only through the
// policy gate and trust level.
package tuning

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Settings configure one agent.
type Settings struct {
	Tuning              config.TuningConfig
	ConfidenceThreshold float64
	Memory              config.OptimizerRejectionMemoryConfig
	// Allowed are the proposal types the agent may make.
	Allowed         map[ProposalType]bool
	CloudEnv        string
	DatabaseName    string
	HostMemoryBytes int64
	MaxOutputTokens int
	Thresholds      Thresholds
	// MaxNewPerTable bounds new index proposals per table per cycle
	// (llm.optimizer.max_new_per_table); 0 means 3.
	MaxNewPerTable int
}

// Deps are the agent's collaborators. Model may be nil (the agent then
// only keeps its open proposals); Fallback is tried when Model fails.
type Deps struct {
	Model    Model
	Fallback Model
	Loop     Loop
	Indexes  IndexTools
	Facts    FactSource
	Hints    HintSink
	Store    Store
	Rehearse Rehearser
	Now      func() time.Time
}

// Agent is one database's tuning agent.
type Agent struct {
	settings   Settings
	deps       Deps
	logFn      func(string, string, ...any)
	memory     *caseMemory
	modelSkips atomic.Int64
	mu         sync.Mutex // one cycle at a time

	tcMu    sync.Mutex
	tcSnap  *collector.Snapshot
	tcCache map[string]tcEntry
}

type tcEntry struct {
	tc optimizer.TableContext
	ok bool
}

// maxTableContexts bounds the table contexts (each a few bounded catalog
// reads) one snapshot may load.
const maxTableContexts = 16

// New returns an agent.
func New(s Settings, d Deps, logFn func(string, string, ...any)) *Agent {
	if d.Loop == nil {
		d.Loop = NewToolLoop()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	ms := caseMemorySettings{}
	if s.Memory.Enabled {
		ms = caseMemorySettings{SkipAfter: s.Memory.SkipLLMAfter,
			MaxAge:    time.Duration(s.Memory.MaxAgeDays) * 24 * time.Hour,
			CallRatio: s.Memory.CallVolumeRatio, MeanRatio: s.Memory.MeanTimeRatio}
	}
	return &Agent{settings: s, deps: d, logFn: logFn, memory: newCaseMemory(ms)}
}

func (a *Agent) now() time.Time { return a.deps.Now() }

// Stats are the agent's counters: what-ifs rejection memory skipped and
// model calls case memory skipped.
func (a *Agent) Stats() analyzer.TuningStats {
	if a == nil {
		return analyzer.TuningStats{}
	}
	s := analyzer.TuningStats{ModelCallsSkipped: a.modelSkips.Load()}
	if a.deps.Indexes != nil {
		s.WhatIfSkipped = a.deps.Indexes.MemoryStats().WhatIfSkipped
	}
	return s
}

// Tune runs one cycle on the interval prev..cur.
func (a *Agent) Tune(ctx context.Context, cur, prev *collector.Snapshot) (
	analyzer.TuningOutput, error) {
	if a == nil {
		return analyzer.TuningOutput{}, errors.New("tuning: no agent")
	}
	if cur == nil {
		return analyzer.TuningOutput{}, errors.New("tuning: no snapshot")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cats := analyzer.TuningCategories()
	if a.deps.Indexes != nil && a.deps.Indexes.ColdStart(ctx) {
		a.logFn("INFO", "tuning: cold start, too little history to judge the workload")
		return analyzer.TuningOutput{Failed: cats}, nil
	}
	all := a.loadFacts(ctx)
	w := ClassifyWorkload(cur, all, a.now())
	cases := DetectCases(cur, prev, w, a.settings.Thresholds)
	open, err := a.openFindings(ctx, cats)
	if err != nil {
		return analyzer.TuningOutput{Failed: cats}, err
	}
	kept, busy := reemit(open, cases)
	out := analyzer.TuningOutput{Evaluated: cats, Findings: kept}
	var ask []Case
	for _, c := range cases {
		if !busy[c.ID] {
			ask = append(ask, c)
		}
	}
	if len(ask) > 0 && a.deps.Model != nil {
		cy := &cycle{cur: cur, prev: prev, w: w, all: all, confirmed: confirmedOnly(all)}
		out.Findings = append(out.Findings, a.askCases(ctx, cy, ask)...)
	}
	out.IndexTables = indexTables(out.Findings)
	return out, nil
}

// loadFacts lists the confirmed and rejected facts; a read error is
// logged and the agent goes on without them (the policy gate still
// binds every action).
func (a *Agent) loadFacts(ctx context.Context) []facts.Fact {
	if a.deps.Facts == nil {
		return nil
	}
	list, err := a.deps.Facts.List(ctx, facts.Filter{Status: []facts.Status{
		facts.StatusConfirmed, facts.StatusRejected}})
	if err != nil {
		a.logFn("WARN", "tuning: facts unreadable, judging without them (the policy gate "+
			"still binds): %v", err)
		return nil
	}
	return list
}

func confirmedOnly(all []facts.Fact) []facts.Fact {
	var out []facts.Fact
	for _, f := range all {
		if f.Status == facts.StatusConfirmed {
			out = append(out, f)
		}
	}
	return out
}

func (a *Agent) openFindings(ctx context.Context, cats []string) ([]analyzer.Finding,
	error) {
	if a.deps.Store == nil {
		return nil, errors.New("tuning: no store")
	}
	open, err := a.deps.Store.OpenFindings(ctx, append(cats, "query_tuning"))
	if err != nil {
		a.logFn("WARN", "tuning: open findings unreadable, nothing resolves this cycle: %v",
			err)
		return nil, err
	}
	return open, nil
}

// tableContext is a table's context for this snapshot, loaded once.
func (a *Agent) tableContext(ctx context.Context, cur *collector.Snapshot,
	table string) (optimizer.TableContext, bool) {
	if a.deps.Indexes == nil {
		return optimizer.TableContext{}, false
	}
	a.tcMu.Lock()
	if a.tcSnap != cur || a.tcCache == nil {
		a.tcSnap, a.tcCache = cur, map[string]tcEntry{}
	}
	e, cached := a.tcCache[table]
	full := len(a.tcCache) >= maxTableContexts
	a.tcMu.Unlock()
	if cached {
		return e.tc, e.ok
	}
	if full {
		a.logFn("DEBUG", "tuning: table context limit reached, %s left out", table)
		return optimizer.TableContext{}, false
	}
	tc, ok, err := a.deps.Indexes.TableContext(ctx, cur, table)
	if err != nil {
		a.logFn("WARN", "tuning: table context of %s: %v", table, err)
		ok = false
	}
	a.tcMu.Lock()
	a.tcCache[table] = tcEntry{tc: tc, ok: ok}
	a.tcMu.Unlock()
	return tc, ok
}
