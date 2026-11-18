package gitops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// Deciding open approval PRs (cli-02b). The row moves first (one
// conditional UPDATE, audited), then the comment and the close are sent;
// a comment that fails is logged, never retried into a second decision.

// decideOpen polls every open PR; a refused token turns PR mode off.
func (p *ApprovalPRs) decideOpen(ctx context.Context) (int, error) {
	rows, err := listApprovalPRs(ctx, p.Pool, PRStateOpen)
	if err != nil {
		return 0, err
	}
	decided := 0
	for _, rec := range rows {
		done, err := p.decideRow(ctx, rec)
		if errors.Is(err, codehost.ErrForbidden) {
			p.disable(err)
			return decided, nil
		}
		if err != nil {
			slog.Warn("approval PR not decided this poll", "database", rec.Database,
				"queue_id", rec.QueueID, "pr", rec.PRNumber, "status", openFailureStatus(err))
			continue
		}
		if done {
			decided++
		}
	}
	return decided, nil
}

// decideRow reads the change and its PR and acts on what changed.
func (p *ApprovalPRs) decideRow(ctx context.Context, rec ApprovalPRRecord) (bool, error) {
	item, err := p.Queue.Item(ctx, rec.Database, rec.QueueID)
	if errors.Is(err, ErrItemNotFound) {
		return p.finish(ctx, rec, PRStateSuperseded, "the change no longer exists in pg_sage",
			true, 0)
	}
	if err != nil {
		return false, err
	}
	st, err := p.Host.PRState(ctx, rec.Repo, rec.PRNumber)
	if err != nil {
		return false, err
	}
	pending := item.Card.Status == "pending"
	switch st.State {
	case codehost.PRMerged:
		return p.decideMerged(ctx, rec, st, item)
	case codehost.PRClosed:
		return p.decideClosed(ctx, rec, pending)
	}
	if !pending {
		return p.finish(ctx, rec, PRStateSuperseded, fmt.Sprintf("already decided in "+
			"pg_sage (%s)", item.Card.Status), true, 0)
	}
	if !p.now().Before(rec.ExpiresAt) {
		return p.finish(ctx, rec, PRStateExpired, fmt.Sprintf("expired at %s; the change "+
			"is still waiting in pg_sage", rec.ExpiresAt.UTC().Format("2006-01-02 15:04 MST")),
			true, 0)
	}
	return false, nil
}

// decideClosed rejects the change of a PR closed unmerged.
func (p *ApprovalPRs) decideClosed(ctx context.Context, rec ApprovalPRRecord,
	pending bool) (bool, error) {
	reason := fmt.Sprintf("closed in %s#%d", rec.Repo, rec.PRNumber)
	if pending {
		err := p.Queue.Reject(ctx, rec.Database, rec.QueueID, 0, reason)
		if err == nil {
			return p.finishQuiet(ctx, rec, PRStateRejected, reason, 0)
		}
		if !errors.Is(err, ErrAlreadyDecided) {
			return false, err
		}
	}
	return p.finishQuiet(ctx, rec, PRStateSuperseded, "already decided in pg_sage", 0)
}

// decideMerged approves the change as the merger, or refuses the merge.
func (p *ApprovalPRs) decideMerged(ctx context.Context, rec ApprovalPRRecord,
	st codehost.PRStatus, item ApprovalItem) (bool, error) {
	if item.Card.Status != "pending" {
		return p.finish(ctx, rec, PRStateSuperseded, fmt.Sprintf("already decided in "+
			"pg_sage (%s)", item.Card.Status), false, 0)
	}
	if refusal, err := p.mergeRefusal(ctx, rec, st, item); err != nil || refusal != "" {
		if err != nil {
			return false, err
		}
		return p.finish(ctx, rec, PRStateRefused, refusal, false, 0)
	}
	approver, err := p.Users.Resolve(ctx, st.MergedBy, rec.Database)
	switch {
	case errors.Is(err, ErrUnmappedLogin):
		return p.finish(ctx, rec, PRStateRefused, fmt.Sprintf("merged by %s, who is not "+
			"mapped to an active pg_sage user in principals/users.yaml", st.MergedBy),
			false, 0)
	case errors.Is(err, ErrNotOperator):
		return p.finish(ctx, rec, PRStateRefused, fmt.Sprintf("merged by %s (%s), who "+
			"lacks the operator role on %s", st.MergedBy, approver.Email, rec.Database),
			false, 0)
	case err != nil:
		return false, err
	}
	outcome, err := p.Queue.Approve(ctx, rec.Database, rec.QueueID, approver.UserID,
		item.Card.SQL)
	switch {
	case errors.Is(err, ErrAlreadyDecided):
		return p.finish(ctx, rec, PRStateSuperseded, "already decided in pg_sage", false, 0)
	case err != nil:
		return p.finish(ctx, rec, PRStateRefused, "pg_sage did not approve it: "+
			err.Error(), false, 0)
	}
	return p.finish(ctx, rec, PRStateApproved, fmt.Sprintf("approved by %s: %s",
		approver.Email, outcome), false, approver.UserID)
}

// mergeRefusal checks the merged file, the card and the tier cap; it
// returns why the merge is not an approval, or "".
func (p *ApprovalPRs) mergeRefusal(ctx context.Context, rec ApprovalPRRecord,
	st codehost.PRStatus, item ApprovalItem) (string, error) {
	mergedAt := p.now()
	if st.MergedAt != nil {
		mergedAt = st.MergedAt.UTC()
	}
	if !mergedAt.Before(rec.ExpiresAt) {
		return "merged after this approval request expired at " +
			rec.ExpiresAt.UTC().Format("2006-01-02 15:04 MST"), nil
	}
	ref := st.MergeSHA
	if ref == "" {
		ref = p.Config.BaseBranch
	}
	var file codehost.File
	var err error
	if ref == "" {
		file, err = p.Host.GetFile(ctx, rec.Repo, rec.Path)
	} else {
		file, err = p.Host.GetFileAt(ctx, rec.Repo, rec.Path, ref)
	}
	switch {
	case errors.Is(err, codehost.ErrNotFound):
		return "the merged approval file " + rec.Path + " is missing", nil
	case err != nil:
		return "", err
	case fileHash(file.Content) != rec.FileHash:
		return "the merged file differs from the file pg_sage opened; an edited " +
			"approval is not accepted", nil
	case rec.CardHash != item.Card.CardHash:
		return "the change changed after this PR was opened; review it again in pg_sage",
			nil
	}
	if ok, reason := p.remoteAllowed(item); !ok {
		return reason, nil
	}
	return "", nil
}

// finish moves the row to its final state, then comments on the PR (and
// closes it when close is set). by is the approver of an approval.
func (p *ApprovalPRs) finish(ctx context.Context, rec ApprovalPRRecord, to, detail string,
	close bool, by int) (bool, error) {
	moved, err := p.finishQuiet(ctx, rec, to, detail, by)
	if err != nil || !moved {
		return moved, err
	}
	if cerr := p.Host.Comment(ctx, rec.Repo, rec.PRNumber, prComment(to, detail)); cerr != nil {
		slog.Warn("approval PR comment not sent", "pr", rec.PRNumber, "status",
			openFailureStatus(cerr))
	}
	if close {
		if cerr := p.Host.ClosePR(ctx, rec.Repo, rec.PRNumber); cerr != nil {
			slog.Warn("approval PR not closed", "pr", rec.PRNumber, "status",
				openFailureStatus(cerr))
		}
	}
	return true, nil
}

// finishQuiet moves the row to its final state without a comment.
func (p *ApprovalPRs) finishQuiet(ctx context.Context, rec ApprovalPRRecord, to,
	detail string, by int) (bool, error) {
	return transition{ID: rec.ID, From: PRStateOpen, To: to, Detail: detail,
		DecidedBy: by, Decided: true}.apply(ctx, p.Pool)
}

// prComment is the comment pg_sage leaves on a decided PR.
func prComment(state, detail string) string {
	switch state {
	case PRStateApproved:
		return "pg_sage: Approved, " + detail + "."
	case PRStateRefused:
		return "pg_sage: Not approved: " + detail + ". The change is still waiting for " +
			"approval in pg_sage."
	case PRStateExpired:
		return "pg_sage: Closed: this approval request " + detail + "."
	default:
		return "pg_sage: Closed: " + detail + "; this pull request no longer decides " +
			"anything."
	}
}
