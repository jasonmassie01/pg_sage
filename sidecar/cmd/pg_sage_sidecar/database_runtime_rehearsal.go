package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/tuning"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Index rehearsal on a clone (lab-04a): with clone.provider set, the
// tuning agent's rehearse tool and the deterministic index floor build a
// proposed index on a disposable, receipted clone of clone.provider,
// connected only after it is shown not to be production, and measure the
// target statements before and after. One rehearser per database, shared
// by both, capped at clone.rehearsals_per_hour.

// indexRehearsalBuildTimeout bounds every statement of the clone session,
// the index build included; each measured statement has its own, shorter
// bound (clone.rehearsal_statement_timeout_seconds).
const indexRehearsalBuildTimeout = 15 * time.Minute

// cloneRehearser is the database's rehearser, built once; nil with
// clone.provider none or a provider that cannot be built (logged).
func (rt *databaseRuntime) cloneRehearser() tuning.Rehearser {
	rt.rehearserOnce.Do(func() {
		reh, err := buildCloneRehearser(cfg, rt.spec.Pool,
			rehearsalReceiptPool(rt.spec.Pool), nil)
		if err != nil {
			logStructuredWrapper("WARN", "clone rehearsal off for %s: clone.provider %s "+
				"could not be built: %v", rt.spec.Name, cfg.Clone.Provider, err)
			return
		}
		rt.rehearser = reh
	})
	return rt.rehearser
}

// buildCloneRehearser is a receipted, production-guarded, capped clone
// rehearser for the database pool reaches; nil for clone.provider none.
// now nil is time.Now.
func buildCloneRehearser(c *config.Config, pool, receipts *pgxpool.Pool,
	now func() time.Time) (tuning.Rehearser, error) {
	if c == nil || c.Clone.Provider == "" || c.Clone.Provider == "none" {
		return nil, nil
	}
	adapter, err := cloneProviderFor(c.Clone, pool)
	if err != nil || adapter == nil {
		return nil, err
	}
	provider, err := receiptingRehearsalProvider(adapter, receipts, c.Clone)
	if err != nil {
		return nil, err
	}
	maxAge := time.Duration(c.Clone.MaxCloneAgeMinutes) * time.Minute
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	reh := tuning.NewCloneRehearser(provider, tuning.RehearsalOptions{
		MaxCloneAge: maxAge, StatementTimeout: indexRehearsalBuildTimeout,
		LockTimeout: migrationLockTimeout,
		AnalyzeTimeout: time.Duration(c.Clone.RehearsalStatementTimeoutSeconds) *
			time.Second,
		Production: clonepkg.PoolIdentity(pool), Provider: c.Clone.Provider,
	})
	return tuning.LimitRehearsals(reh, c.Clone.RehearsalsPerHour, now), nil
}

// floorRehearsal adapts the tuning rehearser to the index floor
// (optimizer cannot import tuning). Each rehearsal ends within bound.
type floorRehearsal struct {
	reh   tuning.Rehearser
	bound time.Duration
}

func (f floorRehearsal) RehearseIndex(ctx context.Context, ddl string,
	queries []optimizer.QueryInfo) verify.LadderRecord {
	if f.bound > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.bound)
		defer cancel()
	}
	req := tuning.RehearsalRequest{DDL: ddl}
	for _, q := range queries {
		req.Statements = append(req.Statements, tuning.RehearsalStatement{
			QueryID: q.QueryID, Text: q.Text})
	}
	res, err := f.reh.Rehearse(ctx, req)
	return tuning.RehearsalLadder(res, err, time.Now())
}

// floorRehearser is the floor's rehearser, or a true nil without one.
func (rt *databaseRuntime) floorRehearser() optimizer.IndexRehearser {
	reh := rt.cloneRehearser()
	if reh == nil {
		return nil
	}
	return floorRehearsal{reh: reh, bound: rehearsalMaxDuration(cfg.Clone)}
}
