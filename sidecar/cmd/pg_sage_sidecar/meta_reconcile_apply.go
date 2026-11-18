package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// metaPlan is one database's pending reconcile change and, for an addition
// or rebuild, the runtime built for it.
type metaPlan struct {
	id        int
	kind      string // added, removed, rebuilt, updated or "" (unchanged)
	rec       store.DatabaseRecord
	connStr   string
	action    memberAction // the stored action identity (security-09b)
	current   *fleet.DatabaseInstance
	candidate *fleet.DatabaseInstance
	buildErr  error
}

func planMetaDatabase(
	ctx context.Context, mgr *fleet.DatabaseManager, state *metaDBState, id int,
) (metaPlan, error) {
	return classifyMetaRecord(ctx, state, id, mgr.GetInstanceByDatabaseID(id))
}

// classifyMetaRecord compares the stored row with the running generation.
func classifyMetaRecord(
	ctx context.Context, state *metaDBState, id int, current *fleet.DatabaseInstance,
) (metaPlan, error) {
	plan := metaPlan{id: id, current: current}
	rec, err := state.Store.Get(ctx, id)
	// A disabled or observe-only member has no runtime (fleetpol-01d).
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !rec.RunsRuntime()) {
		if current != nil {
			plan.kind = "removed"
		}
		return plan, nil
	}
	if err != nil {
		return plan, err
	}
	plan.rec = *rec
	if plan.connStr, err = state.Store.GetConnectionString(ctx, id); err != nil {
		return plan, err
	}
	if plan.action, err = readMemberAction(ctx, state.Store, plan.rec, plan.connStr); err != nil {
		return plan, err
	}
	plan.kind = metaChangeKind(plan.rec, current, plan.connStr)
	if plan.kind != "rebuilt" && metaActionChanged(current, plan.action) {
		plan.kind = "rebuilt" // adding, changing or removing the action identity
	}
	return plan, nil
}

// metaChangeKind classifies a record against its running generation: added,
// rebuilt, updated (applied in place) or "" (unchanged). A tag edit is an
// in-place update whether or not the runtime has an executor, so the next
// fleet-learning cycle sees the new tenant boundary.
func metaChangeKind(
	rec store.DatabaseRecord, current *fleet.DatabaseInstance, connStr string,
) string {
	switch {
	case current == nil:
		return "added"
	case metaNeedsRebuild(rec, current, connStr):
		return "rebuilt"
	case metaPolicyChanged(rec, current):
		return "updated"
	case !config.SameTags(recordTags(rec.Tags), current.ConfigSnapshot().Tags):
		return "updated"
	}
	return ""
}

// metaNeedsRebuild compares the record with the running generation. A
// failed placeholder is rebuilt only when its record changed; otherwise the
// reconnect loop owns its retries.
func metaNeedsRebuild(
	rec store.DatabaseRecord, current *fleet.DatabaseInstance, connStr string,
) bool {
	if current.Pool == nil {
		return !reflect.DeepEqual(storeRecordToDBConfig(rec), current.ConfigSnapshot())
	}
	return rec.Name != current.Name ||
		rec.MaxConnections != current.ConfigSnapshot().MaxConnections ||
		current.Pool.Config().ConnString() != connStr
}

func metaPolicyChanged(rec store.DatabaseRecord, current *fleet.DatabaseInstance) bool {
	if current.Executor == nil {
		return false
	}
	trust := rec.TrustLevel != "" && rec.TrustLevel != current.Executor.TrustLevel()
	return trust || resolveExecMode(rec) != current.Executor.ExecutionMode()
}

// build connects and health-checks the runtime an addition or rebuild
// publishes, outside the lifecycle reservation.
func (p *metaPlan) build(ctx context.Context) {
	if p.kind != "added" && p.kind != "rebuilt" {
		return
	}
	inst, err := prepareStoreDatabaseConnection(ctx, p.rec, p.connStr, p.action)
	if err == nil {
		if checkErr := healthCheckStoreDatabase(ctx, inst); checkErr != nil {
			err = errors.Join(checkErr, cleanupManagedCandidate(inst))
			inst = nil
		}
	}
	p.candidate, p.buildErr = inst, err
}

// discard stops a candidate that was not published.
func (p *metaPlan) discard() {
	if err := cleanupManagedCandidate(p.candidate); err != nil {
		logWarn("meta-db", "db %q: unpublished candidate drain: %v", p.rec.Name, err)
	}
	if p.kind == "added" && fleetMgr != nil && fleetMgr.GetInstance(p.rec.Name) == nil {
		forgetRemovedDatabase(p.rec.Name)
	}
	p.candidate = nil
}

// publish re-reads the row and the running generation inside the
// reservation; when either changed since planning, the pass leaves it to
// the next one.
func (p *metaPlan) publish(
	ctx context.Context, op *fleet.LifecycleMutation, state *metaDBState,
) (metaOutcome, error) {
	fresh, err := classifyMetaRecord(ctx, state, p.id, op.CurrentByDatabaseID(p.id))
	if err != nil {
		return metaOutcome{}, err
	}
	if fresh.kind != p.kind || fresh.current != p.current || fresh.connStr != p.connStr ||
		fresh.action.dsn != p.action.dsn || !reflect.DeepEqual(fresh.rec, p.rec) {
		return metaOutcome{}, nil
	}
	switch p.kind {
	case "removed":
		if err := op.DetachInstance(p.current); err != nil {
			return metaOutcome{}, err
		}
		return metaOutcome{kind: "removed", name: p.current.Name, retired: p.current}, nil
	case "added":
		return p.publishAddition(op)
	case "rebuilt":
		return p.publishRebuild(op)
	}
	if err := applyMetaPolicy(op, p.rec, p.current); err != nil {
		return metaOutcome{}, err
	}
	return metaOutcome{kind: "updated", name: p.rec.Name}, nil
}

// publishAddition publishes the new runtime, or a failed placeholder (as at
// startup) that the reconnect loop retries and the dashboard explains.
func (p *metaPlan) publishAddition(op *fleet.LifecycleMutation) (metaOutcome, error) {
	if err := op.ValidateRegistration(p.rec.Name); err != nil {
		return metaOutcome{}, err
	}
	outcome := metaOutcome{kind: "added", name: p.rec.Name, published: p.candidate}
	if p.buildErr != nil {
		outcome.published = failedStoreInstance(p.rec, p.buildErr.Error())
		outcome.err = p.candidateError()
	}
	if err := op.PublishRegistration(outcome.published); err != nil {
		return metaOutcome{}, err
	}
	return outcome, nil
}

// publishRebuild never trades a working runtime for a broken one; a failed
// placeholder is replaced by an updated placeholder.
func (p *metaPlan) publishRebuild(op *fleet.LifecycleMutation) (metaOutcome, error) {
	outcome := metaOutcome{kind: "rebuilt", name: p.rec.Name, retired: p.current,
		published: p.candidate}
	if p.buildErr != nil {
		if p.current.Pool != nil {
			return metaOutcome{err: fmt.Errorf("%w; keeping the running runtime",
				p.candidateError())}, nil
		}
		outcome.published = failedStoreInstance(p.rec, p.buildErr.Error())
		outcome.err = p.candidateError()
	}
	if err := op.PublishReplacement(p.current.Name, p.current, outcome.published); err != nil {
		return metaOutcome{}, err
	}
	return outcome, nil
}

func (p *metaPlan) candidateError() error {
	return fmt.Errorf("%w: db %q: %w", errMetaRuntimeCandidate, p.rec.Name, p.buildErr)
}

// applyMetaPolicy adopts a record's tags, trust level and execution mode in
// place. sage.databases is the durable policy; the settings API writes it
// and applies it under the same lifecycle reservation. A runtime without an
// executor takes the config (tags included) and has no policy to apply.
func applyMetaPolicy(
	op *fleet.LifecycleMutation, rec store.DatabaseRecord, current *fleet.DatabaseInstance,
) error {
	if err := op.UpdateMetadata(current, storeRecordToDBConfig(rec)); err != nil {
		return err
	}
	if current.Executor == nil {
		return nil
	}
	return applyMetaExecutorPolicy(rec, current)
}

// applyMetaExecutorPolicy moves the executor to the record's trust level and
// execution mode, logging any raise in trust.
func applyMetaExecutorPolicy(rec store.DatabaseRecord, current *fleet.DatabaseInstance) error {
	previous := current.Executor.TrustLevel()
	if rec.TrustLevel != "" {
		if err := current.Executor.SetTrustLevel(rec.TrustLevel); err != nil {
			return fmt.Errorf("db %q: %w", rec.Name, err)
		}
	}
	current.Executor.SetExecutionMode(resolveExecMode(rec))
	current.SetTrustLevelOverride(true)
	level := current.Executor.TrustLevel()
	current.UpdateStatus(func(status *fleet.InstanceStatus) { status.TrustLevel = level })
	if config.TrustRank(level) > config.TrustRank(previous) {
		logWarn("meta-db", "db %q: trust raised %s -> %s from sage.databases",
			rec.Name, previous, level)
	}
	return nil
}

// failedStoreInstance is the dashboard placeholder of a store database
// whose runtime could not be built.
func failedStoreInstance(rec store.DatabaseRecord, errMsg string) *fleet.DatabaseInstance {
	return &fleet.DatabaseInstance{
		Name:       rec.Name,
		DatabaseID: rec.ID,
		Config:     storeRecordToDBConfig(rec),
		Status: &fleet.InstanceStatus{Error: errMsg, LastSeen: time.Now(),
			// Never read, so unknown rather than zero (cases-11).
			IncidentsCritical: fleet.IncidentsUnknown,
			IncidentsWarning:  fleet.IncidentsUnknown},
	}
}
