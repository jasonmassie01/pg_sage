package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// errMetaRuntimeCandidate reports a sage.databases change whose runtime
// could not be built; a running runtime keeps going.
var errMetaRuntimeCandidate = errors.New("meta-db database runtime could not be prepared")

// metaReconcileItemTimeout bounds one database's reconcile step, including
// connecting a new runtime.
const metaReconcileItemTimeout = 30 * time.Second

// metaReconcileReport lists what one reconcile pass changed, by name.
type metaReconcileReport struct {
	Added, Removed, Rebuilt, Updated []string
	Errors                           []error
}

func (r metaReconcileReport) empty() bool {
	return len(r.Added)+len(r.Removed)+len(r.Rebuilt)+len(r.Updated)+len(r.Errors) == 0
}

// metaOutcome is one database's published reconcile result.
type metaOutcome struct {
	kind      string // added, removed, rebuilt or updated
	name      string
	retired   *fleet.DatabaseInstance
	published *fleet.DatabaseInstance
	err       error // published, but with a warning (a failed runtime)
}

// reconcileMetaDatabases converges the fleet on sage.databases, the
// meta-db source of truth: rows added, removed, disabled or changed by
// another replica or by SQL take effect without a restart. A new runtime
// is built outside the lifecycle reservation (connecting may be slow) and
// published inside it only if neither the row nor the running generation
// changed meanwhile, so a pass never races the managed-database API or
// another pass, and never blocks them on an unreachable host.
func reconcileMetaDatabases(
	ctx context.Context, mgr *fleet.DatabaseManager, state *metaDBState,
) metaReconcileReport {
	var report metaReconcileReport
	if mgr == nil || state == nil || state.Store == nil {
		report.Errors = append(report.Errors, errors.New("meta reconcile: no fleet or store"))
		return report
	}
	records, err := state.Store.List(ctx)
	if err != nil {
		report.Errors = append(report.Errors,
			fmt.Errorf("meta reconcile: list databases: %w", err))
		return report
	}
	for _, id := range reconcileIDs(records, mgr.Instances()) {
		reconcileMetaDatabase(ctx, mgr, state, id, &report)
	}
	for _, names := range [][]string{report.Added, report.Removed, report.Rebuilt,
		report.Updated} {
		sort.Strings(names)
	}
	return report
}

func reconcileIDs(
	records []store.DatabaseRecord, instances map[string]*fleet.DatabaseInstance,
) []int {
	seen := map[int]bool{}
	for _, rec := range records {
		seen[rec.ID] = true
	}
	for _, inst := range instances {
		if inst.DatabaseID > 0 {
			seen[inst.DatabaseID] = true
		}
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func reconcileMetaDatabase(
	ctx context.Context, mgr *fleet.DatabaseManager, state *metaDBState,
	id int, report *metaReconcileReport,
) {
	itemCtx, cancel := context.WithTimeout(ctx, metaReconcileItemTimeout)
	defer cancel()
	plan, err := planMetaDatabase(itemCtx, mgr, state, id)
	if err != nil {
		report.Errors = append(report.Errors,
			fmt.Errorf("meta reconcile database %d: %w", id, err))
		return
	}
	if plan.kind == "" {
		return
	}
	plan.build(itemCtx)
	var outcome metaOutcome
	err = mgr.WithLifecycle(itemCtx, func(op *fleet.LifecycleMutation) error {
		var publishErr error
		outcome, publishErr = plan.publish(itemCtx, op, state)
		return publishErr
	})
	if err != nil || outcome.kind == "" {
		plan.discard()
	}
	if err != nil {
		report.Errors = append(report.Errors,
			fmt.Errorf("meta reconcile database %d: %w", id, err))
		return
	}
	finishMetaOutcome(ctx, mgr, outcome, report)
}

// finishMetaOutcome drains the retired generation outside the lifecycle
// reservation and returns a retired name's budget share.
func finishMetaOutcome(
	ctx context.Context, mgr *fleet.DatabaseManager, outcome metaOutcome,
	report *metaReconcileReport,
) {
	if outcome.err != nil {
		report.Errors = append(report.Errors, outcome.err)
	}
	switch outcome.kind {
	case "added":
		report.Added = append(report.Added, outcome.name)
	case "removed":
		report.Removed = append(report.Removed, outcome.name)
	case "rebuilt":
		report.Rebuilt = append(report.Rebuilt, outcome.name)
	case "updated":
		report.Updated = append(report.Updated, outcome.name)
	}
	if outcome.published != nil && outcome.published.Pool != nil {
		updateInstanceFindings(context.Background(), outcome.published)
	}
	if outcome.retired == nil {
		return
	}
	if err := fleet.ShutdownInstance(context.WithoutCancel(ctx), outcome.retired); err != nil {
		logWarn("meta-db", "db %q: retired runtime drain incomplete: %v",
			outcome.retired.Name, err)
	}
	if mgr.GetInstance(outcome.retired.Name) == nil {
		unregisterFleetBudget(outcome.retired.Name)
	}
}

// runMetaReconcilePass logs one reconcile pass of the reconnect loop.
func runMetaReconcilePass(ctx context.Context, state *metaDBState) {
	report := reconcileMetaDatabases(ctx, fleetMgr, state)
	for _, err := range report.Errors {
		logWarn("meta-db", "reconcile: %v", err)
	}
	if len(report.Added)+len(report.Removed)+len(report.Rebuilt)+len(report.Updated) > 0 {
		logInfo("meta-db", "reconcile: added=%v removed=%v rebuilt=%v updated=%v",
			report.Added, report.Removed, report.Rebuilt, report.Updated)
	}
}
