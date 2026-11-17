package main

import (
	"context"
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
)

// drillTarget is inst's scheduler target on its drill adapter (the
// restore command, else clone.provider): its kind (for the opt-ins and
// cadence), the policy's cadence and kinds, whether the agent gate needs a
// fresh drill, and the run itself.
func drillTarget(ctx context.Context, c *config.Config, cloneCfg config.CloneProviderConfig,
	inst drillInstance, doc policy.DrillPolicy, pager *notify.Dispatcher) (drill.Target,
	error) {
	resolved := doc.Resolved()
	adapter, name, scope, err := drillAdapter(c, cloneCfg, inst, resolved.MaxDurationMinutes)
	if err != nil {
		return drill.Target{}, err
	}
	capability, err := clone.DescribeOf(ctx, adapter)
	if err != nil {
		return drill.Target{}, err
	}
	kinds := []clone.Kind(nil)
	for _, k := range resolved.Kinds {
		kinds = append(kinds, clone.Kind(k))
	}
	run := drillRun{c: c, cloneCfg: cloneCfg, inst: inst, adapter: adapter,
		adapterName: name, scope: scope, capability: capability, doc: resolved,
		pager: pager}
	return drill.Target{Database: inst.name, Enabled: true, Kind: capability.Kind,
		AllowFullCopy: cloneCfg.AllowFullCopy, Kinds: kinds,
		CadenceDays:      int(resolved.CadenceDays),
		GateProduction:   agentGateNeedsDrill(ctx, inst.name),
		RequireDrillDays: decide.DefaultRestoreDrillDays, Run: run.run}, nil
}

// agentGateNeedsDrill reports whether the agent gate is on and the
// database is production (or its environment cannot be read: the shorter
// cadence is the safe side), so D7 needs a drill newer than
// require_restore_drill_days.
func agentGateNeedsDrill(ctx context.Context, database string) bool {
	state := agentGate.Load()
	if state == nil {
		return false
	}
	if state.envs == nil {
		return true
	}
	b, err := envSource{svc: state.envs}.Environment(ctx, database)
	return err != nil || b.Env == envbind.EnvProd
}

// drillRun is one database's drill, built when the scheduler starts it.
type drillRun struct {
	c        *config.Config
	cloneCfg config.CloneProviderConfig
	inst     drillInstance
	adapter  clone.Provider
	// adapterName and scope name the adapter's receipts.
	adapterName, scope string
	capability         clone.Capability
	doc                policy.DrillPolicy
	pager              *notify.Dispatcher
}

func (r drillRun) run(ctx context.Context) {
	budget := drill.Budget{MaxDuration: time.Duration(r.doc.MaxDurationMinutes) * time.Minute}
	rate := cloneRateFor(r.c.Cost, r.inst.name)
	results := r.inst.results
	provider, err := drill.NewReceiptedProvider(r.adapter, clone.NewReceiptStore(results),
		drill.ProviderConfig{Adapter: r.adapterName, Scope: r.scope,
			Deployment: func(ctx context.Context) (string, error) {
				return earned.EnsureDeployment(ctx, results)
			},
			Budget: budget, AllowFullCopy: r.cloneCfg.AllowFullCopy,
			MaxCreatesPerHour: r.cloneCfg.MaxCreatesPerHour, Rate: rate, Lifetime: ctx})
	if err != nil {
		logError("drill", "restore drill of %s not started: %v", r.inst.name, err)
		return
	}
	d := drill.Drill{Database: r.inst.name, DatabaseID: r.inst.id, Source: r.inst.pool,
		Provider: provider, ProviderName: r.adapterName, Kind: r.capability.Kind,
		CostLabel:  r.capability.Estimate(drill.ReceiptTTL(budget), rate).Label,
		Invariants: drill.Invariants{MarkerQuery: r.doc.MarkerQuery}, Budget: budget,
		Store: drill.NewStore(results),
		Logf:  func(format string, args ...any) { logInfo("drill", format, args...) }}
	res, err := d.Run(ctx)
	switch {
	case errors.Is(err, drill.ErrDeferred):
		logInfo("drill", "restore drill of %s waits for the next pass: %v", r.inst.name, err)
		return
	case err != nil:
		logError("drill", "restore drill of %s failed: %v", r.inst.name, err)
	case !res.Passed:
		logError("drill", "restore drill of %s failed its checks: %s", r.inst.name,
			res.Failure)
	default:
		logInfo("drill", "restore drill of %s passed (RTO %s)", r.inst.name,
			secondsLabel(res.RTOSeconds))
	}
	if res.Failure == "" && !res.Passed {
		res.Failure = "the drill did not complete"
	}
	if rerr := drill.ReportResult(ctx, r.inst.pool, drillPager(r.pager), res); rerr != nil {
		logWarn("drill", "restore drill finding for %s: %v", r.inst.name, rerr)
	}
}

func secondsLabel(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return (time.Duration(*v * float64(time.Second))).Round(time.Second).String()
}
