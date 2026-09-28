package agentdb

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func liveRunnerFromSource(source any, provider string) (ProviderRunner, bool) {
	registry, ok := source.(*RunnerRegistry)
	if !ok || registry == nil {
		return nil, false
	}
	runner, err := registry.ForProvider(provider)
	if err != nil || runner == nil || runner.Name() == "dry_run" {
		return nil, false
	}
	return runner, true
}

func (s *Store) ReconcileLiveProvisioning(
	ctx context.Context,
	registry *RunnerRegistry,
) (LifecycleReconcileResult, error) {
	if err := s.Ensure(ctx); err != nil {
		return LifecycleReconcileResult{}, err
	}
	lockConn, releaseLock, err := acquireLiveReconcileLock(ctx, s.pool)
	if err != nil {
		return LifecycleReconcileResult{}, err
	}
	defer releaseLock()
	var locked bool
	if err := lockConn.QueryRow(ctx,
		`/* pg_sage */ SELECT pg_try_advisory_lock(hashtext('agentdb-live-reconcile'))`,
	).Scan(&locked); err != nil {
		return LifecycleReconcileResult{}, err
	}
	if !locked {
		return LifecycleReconcileResult{}, ErrRateLimited
	}
	// Materialize the work list before touching other connections so the
	// lock session never holds an open cursor while rows are updated.
	rows, err := lockConn.Query(ctx, selectDeploymentsSQL+`
		WHERE provisioning_level='instance'
			AND provider <> $1
			AND provisioning_status IN (
				'provisioning', 'create_uncertain', 'destroy_pending', 'destroying',
				'status_unknown'
			)`,
		ProviderLocalPostgres,
	)
	if err != nil {
		return LifecycleReconcileResult{}, err
	}
	pending, err := scanDeployments(rows)
	rows.Close()
	if err != nil {
		return LifecycleReconcileResult{}, err
	}
	result := LifecycleReconcileResult{}
	for _, dep := range pending {
		if err := s.reconcileLiveRow(ctx, registry, dep, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Store) reconcileLiveRow(
	ctx context.Context,
	registry *RunnerRegistry,
	dep Deployment,
	result *LifecycleReconcileResult,
) error {
	runner, err := registry.ForProvider(dep.Provider)
	if err != nil || runner.Name() == "dry_run" {
		appendLifecycleBlock(result, dep.DeploymentID, "live runner unavailable")
		return nil
	}
	if dep.Status != "deleted" && resumableTeardown(dep) {
		attempt, destroyErr := s.destroyAuthorizedLive(ctx, dep, runner)
		if destroyErr != nil {
			s.blockTeardown(ctx, result, dep.DeploymentID, teardownBlockReason(destroyErr))
			return nil
		}
		result.DestroyLive = append(result.DestroyLive, attempt)
		return nil
	}
	if dep.ProvisioningStatus == "destroy_pending" {
		appendLifecycleBlock(result, dep.DeploymentID,
			"destroy pending without a teardown operation")
		return nil
	}
	return s.reconcileLiveStatus(ctx, runner, dep, result)
}

// reconcileLiveStatus refreshes provider state. For an uncertain create it
// adopts a tag-verified resource (recording the live receipt) or, when the
// provider confirms nothing exists, marks the create definitively failed.
func (s *Store) reconcileLiveStatus(
	ctx context.Context,
	runner ProviderRunner,
	dep Deployment,
	result *LifecycleReconcileResult,
) error {
	status := runner.Status(ctx, ProvisionRequest{
		Operation: ProvisionOpStatus, Deployment: dep,
		Plan: dep.ProvisioningPlan, RequestedAt: time.Now().UTC(),
	})
	next, adopt, blockReason := liveStatusOutcome(dep, status)
	if blockReason != "" {
		appendLifecycleBlock(result, dep.DeploymentID, blockReason)
		return nil
	}
	attempt, err := s.recordProvisionAttempt(ctx, dep.DeploymentID, provisionAttemptInput{
		Kind: "live_reconcile_status", Status: "succeeded", Runner: runner.Name(),
		Detail: RedactProviderDetail(status.Detail), FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	result.StatusChecked = append(result.StatusChecked, attempt)
	if adopt {
		if err := s.RecordCreationReceipt(ctx, CreationReceipt{
			DeploymentID: dep.DeploymentID, Provider: dep.Provider,
			ProviderResourceID: status.ProviderResourceID, OperationMode: "live",
			RequestHash: dep.CreateOperationID,
			Detail:      map[string]any{"adopted_by": "live_reconcile_status"},
		}); err != nil {
			return err
		}
	}
	status.Error = nil
	if err := s.applyProvisionResult(ctx, dep.DeploymentID, next, status, adopt); err != nil {
		appendLifecycleBlock(result, dep.DeploymentID, err.Error())
		return nil
	}
	if next == "failed" {
		return s.clearFailedLiveCreate(ctx, dep.DeploymentID)
	}
	return nil
}

func liveStatusOutcome(dep Deployment, status ProvisionResult) (string, bool, string) {
	notFound := errors.Is(publicProviderError(status.Error), ErrNotFound)
	uncertain := dep.ProvisioningStatus == "create_uncertain" ||
		(dep.ProviderResourceID == "" && dep.CreateOperationID != "")
	switch {
	case status.Error == nil:
		adopt := uncertain && status.ProviderResourceID != "" && dep.ProviderResourceID == ""
		return firstNonEmpty(status.Status, "status_checked"), adopt, ""
	case notFound && uncertain && dep.ProviderResourceID == "":
		return "failed", false, ""
	case notFound && dep.ProvisioningStatus == "destroying":
		return "destroyed", false, ""
	default:
		return "", false, status.Error.Error()
	}
}

func acquireLiveReconcileLock(
	ctx context.Context,
	pool *pgxpool.Pool,
) (*pgxpool.Conn, func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	release := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		err := conn.QueryRow(cleanupCtx,
			`/* pg_sage */ SELECT pg_advisory_unlock(`+
				`hashtext('agentdb-live-reconcile'))`,
		).Scan(&unlocked)
		if err == nil && unlocked {
			conn.Release()
			return
		}
		raw := conn.Hijack()
		_ = raw.Close(cleanupCtx)
	}
	return conn, release, nil
}

func commandRunnerFromSource(source any, provider string) (ProvisionRunner, error) {
	switch typed := source.(type) {
	case nil:
		return DryRunProvisionRunner{}, nil
	case ProvisionRunner:
		return typed, nil
	case *RunnerRegistry:
		return typed.CommandRunnerForProvider(provider)
	default:
		return nil, ErrInvalid
	}
}

func appendLifecycleBlock(result *LifecycleReconcileResult, id, reason string) {
	result.Blocked = append(result.Blocked, LifecycleBlocked{
		DeploymentID: id,
		Reason:       reason,
	})
}
