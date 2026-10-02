package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// ErrTargetLeased refuses an operator action whose object another action
// holds (serialize_mode park, or a queue wait past its bound). The
// operator retries once the holder is done.
var ErrTargetLeased = errors.New("target object is being changed by another action")

// TargetLease is a typed-target change lease: the exact objects (OID, name
// and kind, resolved from the catalog) an action changes. Two leases on
// one object never run together; the policy's serialize_mode decides
// whether the later one parks (or, for an operator, is refused) or waits
// its turn in the lease queue.
type TargetLease struct {
	// Kind is finding, custodian, operator or retention.
	Kind string
	// Actor is recorded as the holder (an operator's user, the custodian).
	Actor string
	// Targets are object names as the statement writes them.
	Targets []string
	Intent  string
	// Operator marks a human action: a conflict refuses it with the holder
	// named instead of parking it for the next cycle.
	Operator bool
}

// WithLeaseQueue sets the lease queue bounds (serialize_mode queue); zero
// fields keep the defaults.
func (e *Executor) WithLeaseQueue(cfg policy.LeaseQueueConfig) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.leaseQueueCfg = cfg
}

func (e *Executor) leaseQueue() *policy.LeaseQueue {
	e.policyMu.RLock()
	cfg := e.leaseQueueCfg
	e.policyMu.RUnlock()
	return policy.NewLeaseQueue(e.pool, nil, cfg, policy.ProcessInstance())
}

// leaseSpecFor is the lease an intent takes: its explicit typed-target
// lease, or for a finding or custodian change, its targets when the
// statement is a leased mutation.
func leaseSpecFor(intent ActionIntent) (TargetLease, bool) {
	if intent.TargetLease != nil {
		return *intent.TargetLease, true
	}
	if intent.Lease == nil || !isLeasedMutation(intent.Lease.RecommendedSQL) {
		return TargetLease{}, false
	}
	kind, actor := "finding", "executor"
	if intent.Lease.ObjectType == "custodian" {
		kind, actor = "custodian", "custodian"
	}
	targets := intent.Request.TargetObjs
	if len(targets) == 0 {
		targets = targetObjectsForFinding(*intent.Lease)
	}
	return TargetLease{Kind: kind, Actor: actor, Targets: append([]string(nil), targets...),
		Intent: intent.Lease.RecommendedSQL}, true
}

// isLeasedMutation reports statements that hold their object while they
// run: every DDL mutation, and VACUUM (a custodian freeze can hold a table
// for minutes). ANALYZE, settings and signals take no lease.
func isLeasedMutation(sql string) bool {
	if isDDLMutation(sql) {
		return true
	}
	upper := strings.ToUpper(strings.TrimSpace(sql))
	return upper == "VACUUM" || strings.HasPrefix(upper, "VACUUM ") ||
		strings.HasPrefix(upper, "VACUUM(")
}

// intentLease takes the intent's lease after its first authorization. A
// lease needs a recorded decision. It returns the release and whether a
// lease is held.
func (e *Executor) intentLease(
	ctx context.Context, intent ActionIntent, decision ActionPolicyDecision,
) (func(), bool, error) {
	spec, ok := leaseSpecFor(intent)
	if !ok || decision.DecisionID <= 0 {
		return func() {}, false, nil
	}
	release, held, err := e.acquireTargetLease(ctx, spec, decision)
	if err != nil {
		return nil, false, e.leaseRefused(ctx, intent, spec, decision.DecisionID, err)
	}
	return release, held, nil
}

// acquireTargetLease resolves the targets and takes their lease, waiting
// in the lease queue when the policy serializes by queue. Targets with no
// catalog object and no qualified name take no lease.
func (e *Executor) acquireTargetLease(
	ctx context.Context, spec TargetLease, decision ActionPolicyDecision,
) (func(), bool, error) {
	if e.pool == nil {
		return nil, false, errors.New("change lease: database pool unavailable")
	}
	targets, err := e.resolveLeaseTargets(ctx, spec.Targets)
	if err != nil {
		return nil, false, err
	}
	if len(targets) == 0 {
		return func() {}, false, nil
	}
	manager := policy.NewPostgresLeaseManager(e.pool, nil, decision.DecisionID,
		e.ddlTimeout()+time.Minute)
	var leaseID policy.LeaseID
	if decision.SerializeMode == policy.SerializeQueue {
		leaseID, err = e.leaseQueue().Acquire(ctx, policy.QueuedLeaseRequest{
			Manager: manager, Kind: spec.Kind, Actor: spec.Actor, Intent: spec.Intent,
			Targets: targets, Operator: spec.Operator,
		})
	} else {
		leaseID, err = manager.AcquireTyped(ctx, spec.Actor, targets, spec.Intent)
	}
	if err != nil {
		return nil, false, err
	}
	return func() {
		if err := manager.ReleaseLease(context.WithoutCancel(ctx), leaseID); err != nil {
			e.logFn("executor", "release change lease %s on %v: %v", leaseID,
				spec.Targets, err)
		}
	}, true, nil
}

// resolveLeaseTargets reads the catalog within the lease connection wait:
// an exhausted pool is a busy lease (the action parks), not a stall.
func (e *Executor) resolveLeaseTargets(
	ctx context.Context, names []string,
) ([]policy.TypedTarget, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, policy.LeaseConnectionWait)
	defer cancel()
	targets, err := policy.ResolveTypedTargets(resolveCtx, e.pool, names)
	if err == nil {
		return targets, nil
	}
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w (resolving %v)", policy.ErrLeaseBusy, names)
	}
	return nil, fmt.Errorf("resolve change lease targets: %w", err)
}

// leaseRefused records a lease the action could not take. A conflict
// refuses an operator action (recorded as blocked, holder named) and parks
// a self-initiated one (recorded as parked, no failed action). Any other
// error is the intent's refusal (a failed action for a finding).
func (e *Executor) leaseRefused(
	ctx context.Context, intent ActionIntent, spec TargetLease, decisionID int64, err error,
) error {
	if spec.Operator && isLeaseConflict(err) {
		e.recordLeaseVerdict(ctx, decisionID, leaseVerdictRefused, err)
		return fmt.Errorf("%w: %w", ErrTargetLeased, err)
	}
	finding := analyzer.Finding{Title: spec.Intent}
	if intent.Lease != nil {
		finding = *intent.Lease
	}
	if !e.parkLeaseConflict(ctx, finding, decisionID, err) && intent.Refused != nil {
		intent.Refused(ctx, decisionID, err)
	}
	return fmt.Errorf("acquire change lease: %w", err)
}
