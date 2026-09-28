package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// migrateRecommendations maps the database's pre-existing open findings
// and queued approvals into the durable recommendation model. It is
// idempotent and runs on every start, so a failure is retried next time;
// until then new recommendations still work.
func migrateRecommendations(ctx context.Context, spec databaseRuntimeSpec) {
	report, err := recommendation.MigrateLegacy(ctx, spec.Pool, spec.Name)
	if err != nil {
		logError(spec.Scope, "db %q: recommendation migration incomplete (%d findings, "+
			"%d queued, %d approvals mapped): %v", spec.Name, report.Findings,
			report.QueueLinked, report.ApprovalsMigrated, err)
		return
	}
	if report.Findings+report.QueueLinked > 0 {
		logInfo(spec.Scope, "db %q: recommendations migrated: %d findings, %d queued "+
			"actions, %d approvals kept, %d skipped", spec.Name, report.Findings,
			report.QueueLinked, report.ApprovalsMigrated, report.Skipped)
	}
}

// policyVersionReader reads the version of the standing policy the
// executor enforces for this database (the same store and scope), for
// the analyzer to record on each recommendation revision.
func (rt *databaseRuntime) policyVersionReader() func(context.Context) (int64, error) {
	pool := rt.spec.ControlPool
	if pool == nil {
		pool = rt.spec.Pool
	}
	store := policy.NewStore(pool)
	var scope policy.Scope
	if rt.spec.DatabaseID > 0 {
		id := int64(rt.spec.DatabaseID)
		scope.DatabaseID = &id
	}
	return func(ctx context.Context) (int64, error) {
		current, err := store.Current(ctx, scope)
		return current.Version, err
	}
}
