package main

import (
	"context"
	"sort"

	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// runAgentClassScan proposes column classes in every monitored database
// (spec §6.7: new columns are detected and classes proposed; no grant
// follows until an operator confirms). Rules always run; the model adds
// proposals when one is configured (§6.16), off the decision path. Each
// database's pass is bounded and resumes from its cursor the next day.
func runAgentClassScan(ctx context.Context, mgr *fleet.DatabaseManager,
	lead governanceLeader) {
	if _, ok := lead.fence("agent column classification"); !ok || mgr == nil {
		return
	}
	model := agentClassModel()
	names := make([]string, 0)
	for name, inst := range mgr.Instances() {
		if inst != nil && inst.Pool != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		inst := mgr.GetInstance(name)
		if inst == nil || inst.Pool == nil {
			continue
		}
		scanDatabaseClasses(ctx, name, classify.NewStore(inst.Pool), model)
	}
}

// agentClassModel is the general model client, nil without one.
func agentClassModel() classify.Suggester {
	if llmMgr == nil || llmMgr.General == nil || !llmMgr.General.IsEnabled() {
		return nil
	}
	return classify.NewModelSuggester(llmMgr.General)
}

func scanDatabaseClasses(ctx context.Context, name string, store *classify.Store,
	model classify.Suggester) {
	if n, err := store.ExpireDropped(ctx); err != nil {
		logWarn("agents", "db %q: expiring classes of dropped columns failed: %v", name, err)
	} else if n > 0 {
		logInfo("agents", "db %q: %d column classes expired (columns dropped)", name, n)
	}
	agentClassCursorMu.Lock()
	cursor := agentClassCursors[name]
	agentClassCursorMu.Unlock()
	proposed, modelFailed := 0, false
	for range agentClassMaxPages {
		res, err := classify.Scan(ctx, store, classify.ScanOptions{After: cursor,
			Model: model, Seen: agentClassSeen})
		if err != nil {
			logWarn("agents", "db %q: column classification scan failed: %v", name, err)
			break
		}
		if res.ModelErr != nil && !modelFailed {
			modelFailed = true
			logWarn("agents", "db %q: model classification proposals failed; name rules "+
				"still applied: %v", name, res.ModelErr)
		}
		proposed += res.Proposed
		cursor = res.Next
		if res.Done {
			break
		}
	}
	agentClassCursorMu.Lock()
	agentClassCursors[name] = cursor
	agentClassCursorMu.Unlock()
	if proposed > 0 {
		logInfo("agents", "db %q: %d column classes proposed; confirm them before agents "+
			"get those columns in stage or prod", name, proposed)
	}
}
