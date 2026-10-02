package executor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/value"
)

// MaxBackendEvidenceAge is the oldest identity evidence a backend cancel
// may act on (AI-SRE-SPEC R1.1). Configuration may only tighten it.
const MaxBackendEvidenceAge = 5 * time.Second

// ErrInvalidBackendCancel reports a malformed cancel request; nothing was
// authorized or signalled.
var ErrInvalidBackendCancel = errors.New("invalid backend cancel request")

// BackendIdentity pins one statement of one session: pid + backend_start
// (PIDs are reused), the statement's query_start, and the database, user,
// query hash (sha256 of the statement text) and query_id it runs with.
type BackendIdentity struct {
	PID          int32
	BackendStart time.Time
	QueryStart   time.Time
	Database     string
	User         string
	QueryHash    string
	QueryID      int64
}

// Validate checks the identity is complete.
func (b BackendIdentity) Validate() error {
	hash, err := hex.DecodeString(b.QueryHash)
	switch {
	case b.PID <= 0:
		return fmt.Errorf("%w: pid %d", ErrInvalidBackendCancel, b.PID)
	case b.BackendStart.IsZero() || b.QueryStart.IsZero():
		return fmt.Errorf("%w: backend_start and query_start are required",
			ErrInvalidBackendCancel)
	case b.QueryStart.Before(b.BackendStart):
		return fmt.Errorf("%w: query started before its session", ErrInvalidBackendCancel)
	case strings.TrimSpace(b.Database) == "" || strings.TrimSpace(b.User) == "":
		return fmt.Errorf("%w: database and user are required", ErrInvalidBackendCancel)
	case err != nil || len(hash) != 32:
		return fmt.Errorf("%w: query hash must be a sha256 hex digest",
			ErrInvalidBackendCancel)
	}
	return nil
}

// BackendCancel is one human-approved cancel of one backend.
type BackendCancel struct {
	Target BackendIdentity
	// ObservedAt is when the identity evidence was sampled; the signal is
	// refused once it is older than MaxEvidenceAge.
	ObservedAt time.Time
	// MaxEvidenceAge is in (0, MaxBackendEvidenceAge]; 0 means the ceiling.
	MaxEvidenceAge time.Duration
	// FindingID is the approval item's finding; the action is logged on it.
	FindingID  int
	ApprovedBy int
	IsReplica  bool
	// ProtectedRoles and ProtectedApplications are operator-designated
	// critical backends (exact names) that are never signalled.
	ProtectedRoles        []string
	ProtectedApplications []string
	// Evidence links the decision to its investigation and proposal.
	Evidence map[string]any
}

func (r BackendCancel) maxAge() (time.Duration, error) {
	switch {
	case r.MaxEvidenceAge < 0 || r.MaxEvidenceAge > MaxBackendEvidenceAge:
		return 0, fmt.Errorf("%w: evidence age bound %s outside (0, %s]",
			ErrInvalidBackendCancel, r.MaxEvidenceAge, MaxBackendEvidenceAge)
	case r.ObservedAt.IsZero():
		return 0, fmt.Errorf("%w: the evidence has no observation time",
			ErrInvalidBackendCancel)
	case r.FindingID <= 0:
		return 0, fmt.Errorf("%w: no approval finding", ErrInvalidBackendCancel)
	case r.MaxEvidenceAge == 0:
		return MaxBackendEvidenceAge, r.Target.Validate()
	}
	return r.MaxEvidenceAge, r.Target.Validate()
}

// CancelBackend runs one approved, evidence-matched cancel through Apply:
// the standing gate authorizes it as an operator-approved, mitigation-only
// backend signal (and re-authorizes it after any wait), the evidence age
// is checked at the signal, and the signalling statement rechecks the
// whole identity. It returns the action_log id. A target that no longer
// matches is ErrBackendEvidenceStale and is never signalled.
func (e *Executor) CancelBackend(ctx context.Context, req BackendCancel) (int64, error) {
	if req.ApprovedBy <= 0 {
		return 0, ErrBackendApprovalRequired
	}
	maxAge, err := req.maxAge()
	if err != nil {
		return 0, err
	}
	run := &backendCancelRun{executor: e, req: req, maxAge: maxAge}
	runCtx, cancel := e.detachedDDLContext(ctx)
	defer cancel()
	// A signal is not DDL: it takes no DDL slot and no change lease.
	return e.Apply(runCtx, ActionIntent{Authorize: run.authorize, SlotHeld: true,
		Execute: run.execute})
}

// PreviewBackendCancel is the verdict an approved cancel would get now,
// recorded nowhere (for the proposal's policy verdict).
func (e *Executor) PreviewBackendCancel(ctx context.Context, isReplica bool) ActionPolicyDecision {
	req := cancelPolicyRequest(BackendCancel{Target: BackendIdentity{PID: 1},
		IsReplica: isReplica})
	contract := CancelBackendRepairContract().ActionContract
	explainer, ok := e.StandingPolicyGate().(policy.Explainer)
	if !ok {
		return noStandingPolicyDecision(contract)
	}
	return standingPolicyDecision(explainer.Explain(ctx, req))
}

// backendCancelRun is one cancel moving through Apply.
type backendCancelRun struct {
	executor *Executor
	req      BackendCancel
	maxAge   time.Duration
	authed   int
}

func (r *backendCancelRun) authorize(ctx context.Context) (ActionPolicyDecision, error) {
	reauthorize := r.authed > 0
	r.authed++
	gate := r.executor.StandingPolicyGate()
	if gate == nil {
		d := noStandingPolicyDecision(CancelBackendRepairContract().ActionContract)
		return d, &WithheldError{Decision: d, Reauthorized: reauthorize}
	}
	d := standingPolicyDecision(gate.Authorize(ctx, cancelPolicyRequest(r.req)))
	if d.Decision != PolicyDecisionExecute {
		return d, &WithheldError{Decision: d, Reauthorized: reauthorize}
	}
	return d, nil
}

// execute re-checks the hard stops and the evidence age, signals the exact
// identity and records the action.
func (r *backendCancelRun) execute(ctx context.Context,
	decision ActionPolicyDecision) (int64, error) {
	e := r.executor
	if err := e.manualMutationBlock(ctx); err != nil {
		return 0, err
	}
	if age := time.Since(r.req.ObservedAt); age > r.maxAge {
		return 0, fmt.Errorf("%w: identity evidence is %s old (limit %s)",
			ErrBackendEvidenceStale, age.Round(time.Millisecond), r.maxAge)
	}
	if err := e.signalIdentity(ctx, r.req); err != nil {
		return 0, err
	}
	return r.record(ctx, decision)
}

// record logs the sent cancel. Its outcome says the signal was delivered;
// recovery is verified separately and closes the verification.
func (r *backendCancelRun) record(ctx context.Context,
	decision ActionPolicyDecision) (int64, error) {
	e, t := r.executor, r.req.Target
	approvedBy := r.req.ApprovedBy
	before := map[string]any{"pid": t.PID, "backend_start": t.BackendStart,
		"query_start": t.QueryStart, "database": t.Database, "user": t.User,
		"query_hash": t.QueryHash, "query_id": t.QueryID, "evidence": r.req.Evidence}
	actionID := e.logManualActionWithDecision(ctx, r.req.FindingID, cancelSQL(t.PID), "",
		before, nil, &approvedBy, decision.DecisionID, nil)
	if actionID <= 0 {
		return 0, fmt.Errorf("the cancel of pid %d was sent but could not be recorded", t.PID)
	}
	if _, err := e.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.action_log
		SET outcome = 'success', measured_at = now() WHERE id = $1`, actionID); err != nil {
		e.logFn("executor", "cancel of pid %d (action %d) sent; marking it delivered "+
			"failed: %v", t.PID, actionID, err)
	}
	return actionID, nil
}

// signalIdentity sends pg_cancel_backend only if the backend still matches
// the whole identity, in the same statement.
func (e *Executor) signalIdentity(ctx context.Context, req BackendCancel) error {
	t := req.Target
	var signalled bool
	err := e.pool.QueryRow(ctx, cancelBackendIdentitySQL, t.PID, t.BackendStart,
		t.QueryStart, t.Database, t.User, t.QueryHash, t.QueryID,
		nonNil(req.ProtectedRoles), nonNil(req.ProtectedApplications)).Scan(&signalled)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !signalled) {
		return fmt.Errorf("%w: backend %d no longer matches its approved identity",
			ErrBackendEvidenceStale, t.PID)
	}
	if err != nil {
		return fmt.Errorf("signalling validated backend %d: %w", t.PID, err)
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func cancelSQL(pid int32) string { return fmt.Sprintf("SELECT pg_cancel_backend(%d)", pid) }

// cancelPolicyRequest is the gate request of an approved cancel.
func cancelPolicyRequest(req BackendCancel) policy.ActionRequest {
	contract := CancelBackendRepairContract().ActionContract
	evidence := map[string]any{"source": "sre_action", "approved_by": req.ApprovedBy,
		"finding_id": req.FindingID}
	for k, v := range req.Evidence {
		if _, reserved := evidence[k]; !reserved {
			evidence[k] = v
		}
	}
	return policy.ActionRequest{SQL: cancelSQL(req.Target.PID),
		Feature: changeClassForActionType(contract.ActionType), Contract: policyContract(contract),
		TargetObjs: []string{fmt.Sprintf("pid:%d", req.Target.PID)}, Evidence: evidence,
		IsReplica: req.IsReplica, OperatorApproved: true}
}

// cancelBackendIdentitySQL cancels exactly one statement: every identity
// column must still match, the backend must be an active client backend
// of this database, and protected backends never match.
const cancelBackendIdentitySQL = `/* pg_sage */
SELECT pg_catalog.pg_cancel_backend(a.pid)
  FROM pg_catalog.pg_stat_activity AS a
 WHERE a.pid = $1
   AND a.backend_start = $2
   AND a.query_start = $3
   AND a.datname = $4
   AND a.datname = pg_catalog.current_database()
   AND a.usename = $5
   AND pg_catalog.encode(pg_catalog.sha256(convert_to(a.query, 'UTF8')), 'hex') = $6
   AND COALESCE(a.query_id, 0) = $7
   AND a.state = 'active'
   AND a.backend_type = 'client backend'
   AND a.pid <> pg_catalog.pg_backend_pid()
   AND a.usename <> ALL ($8::text[])
   AND a.application_name <> ALL ($9::text[])
   AND a.application_name NOT ILIKE '%pg_sage%'
   AND a.application_name NOT ILIKE '%pg_dump%'
   AND a.application_name NOT ILIKE '%pg_basebackup%'
   AND a.application_name NOT ILIKE '%pg_restore%'`

// RecordRecoveryVerdict closes the executor-side verification of a backend
// cancel with the Sage SRE recovery verdict (success, failed or
// unverifiable). Only a verified recovery earns value credit.
func (e *Executor) RecordRecoveryVerdict(ctx context.Context, actionID int64,
	verdict, reason string) error {
	switch verdict {
	case "success", "failed", "unverifiable":
	default:
		return fmt.Errorf("%w: recovery verdict %q", ErrInvalidBackendCancel, verdict)
	}
	id, err := finalizeActionVerification(ctx, e.pool, actionID, verdict, reason)
	if err != nil {
		return fmt.Errorf("recording recovery verdict for action %d: %w", actionID, err)
	}
	if verdict == "success" && id > 0 {
		e.creditVerifiedAction(ctx, actionID)
	}
	return nil
}

func (e *Executor) creditVerifiedAction(ctx context.Context, actionID int64) {
	repo := value.NewPostgresRepository(e.pool)
	if _, err := value.NewService(repo).CreditVerifiedAction(ctx, actionID); err != nil {
		e.logFn("executor", "crediting the verified cancel (action %d) failed: %v",
			actionID, err)
	}
}
