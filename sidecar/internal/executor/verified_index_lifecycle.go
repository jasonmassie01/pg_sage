package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

var ErrVerificationUnavailable = errors.New("index verification unavailable")

type verifiedIndexAction struct {
	SQL         string
	RollbackSQL string
	WatchID     string
	Table       string
	IndexName   string
	QueryIDs    []int64
	Criterion   verify.Criterion
}

type resumedIndexVerification struct {
	ActionID    int64
	RollbackSQL string
	Verdict     verify.Verdict
}

type indexVerifier interface {
	OKToApplyNow(context.Context) (verify.Admission, error)
	Watch(context.Context, verify.WatchRequest) (verify.Verdict, error)
	ResumeDue(context.Context) ([]resumedIndexVerification, error)
}

type verifiedIndexActions interface {
	Apply(context.Context, verifiedIndexAction) (int64, error)
	Retain(context.Context, int64, verify.Verdict) error
	Revert(context.Context, int64, string, verify.Verdict) error
}

type verifiedIndexLifecycle struct {
	verifier indexVerifier
	actions  verifiedIndexActions
	now      func() time.Time
}

func newVerifiedIndexLifecycle(
	verifier indexVerifier, actions verifiedIndexActions,
) *verifiedIndexLifecycle {
	return &verifiedIndexLifecycle{verifier: verifier, actions: actions, now: time.Now}
}

func (l *verifiedIndexLifecycle) Apply(
	ctx context.Context, action verifiedIndexAction,
) error {
	if err := l.Admit(ctx); err != nil {
		return err
	}
	actionID, err := l.actions.Apply(ctx, action)
	if err != nil {
		return fmt.Errorf("apply verified index action: %w", err)
	}
	return l.WatchApplied(ctx, action, actionID)
}

func (l *verifiedIndexLifecycle) Admit(ctx context.Context) error {
	if l == nil || l.verifier == nil || l.actions == nil {
		return ErrVerificationUnavailable
	}
	admission, err := l.verifier.OKToApplyNow(ctx)
	if err != nil {
		return fmt.Errorf("%w: load admission: %v", ErrVerificationUnavailable, err)
	}
	if !admission.OK {
		return fmt.Errorf("%w: %s", ErrVerificationUnavailable, admission.Reason)
	}
	return nil
}

func (l *verifiedIndexLifecycle) WatchApplied(
	ctx context.Context, action verifiedIndexAction, actionID int64,
) error {
	if l == nil || l.verifier == nil || l.actions == nil || actionID <= 0 {
		return ErrVerificationUnavailable
	}
	request := watchRequest(action, actionID, l.now())
	verdict, err := l.verifier.Watch(ctx, request)
	if err != nil {
		return l.revertUnverifiable(ctx, action, actionID, err)
	}
	return l.finalize(ctx, actionID, action.RollbackSQL, verdict)
}

func (l *verifiedIndexLifecycle) ResumeDue(ctx context.Context) error {
	if l == nil || l.verifier == nil || l.actions == nil {
		return ErrVerificationUnavailable
	}
	results, err := l.verifier.ResumeDue(ctx)
	for _, result := range results {
		if finalizeErr := l.finalize(
			ctx, result.ActionID, result.RollbackSQL, result.Verdict,
		); finalizeErr != nil {
			err = errors.Join(err, finalizeErr)
		}
	}
	if err != nil {
		return fmt.Errorf("resume index verification: %w", err)
	}
	return nil
}

func (l *verifiedIndexLifecycle) finalize(
	ctx context.Context, actionID int64, rollbackSQL string, verdict verify.Verdict,
) error {
	switch {
	case verdict.Retain:
		return l.actions.Retain(ctx, actionID, verdict)
	case verdict.Revert:
		return l.actions.Revert(ctx, actionID, rollbackSQL, verdict)
	case verdict.Status == "extended" || verdict.Status == "pending":
		return nil
	default:
		verdict.Revert = true
		verdict.Reason = "unverifiable_verdict"
		return l.actions.Revert(ctx, actionID, rollbackSQL, verdict)
	}
}

func (l *verifiedIndexLifecycle) revertUnverifiable(
	ctx context.Context, action verifiedIndexAction, actionID int64, cause error,
) error {
	verdict := verify.Verdict{
		Revert: true, Status: "unverifiable", Reason: "observation_unavailable",
	}
	if err := l.actions.Revert(ctx, actionID, action.RollbackSQL, verdict); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("verify applied index: %w", cause)
}

func watchRequest(
	action verifiedIndexAction, actionID int64, executedAt time.Time,
) verify.WatchRequest {
	criterion := action.Criterion
	criterion.TargetIDs = append([]int64(nil), action.QueryIDs...)
	return verify.WatchRequest{
		ID: action.WatchID, ActionID: actionID, ExecutedAt: executedAt,
		Table: action.Table, IndexName: action.IndexName, Criterion: criterion,
	}
}
