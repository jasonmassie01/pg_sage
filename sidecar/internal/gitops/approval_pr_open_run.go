package gitops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// openPending opens a PR for every pending change that has none and may
// be approved remotely, then retries the failed opens whose backoff
// passed. It stops at the first refused token.
func (p *ApprovalPRs) openPending(ctx context.Context) (opened, failed int, err error) {
	items, err := p.Queue.Pending(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("gitops: listing changes waiting for approval: %w", err)
	}
	byKey := map[string]ApprovalItem{}
	for _, item := range items {
		byKey[itemKey(item.Card.Database, item.Card.QueueID)] = item
		if ok, _ := p.remoteAllowed(item); !ok || item.Card.Status != "pending" {
			continue
		}
		rec, claimed, err := p.claim(ctx, item)
		if err != nil {
			return opened, failed, err
		}
		if claimed {
			o, f := p.openOne(ctx, rec, item)
			opened, failed = opened+o, failed+f
			if p.Disabled() != "" {
				return opened, failed, nil
			}
		}
	}
	o, f, err := p.retryFailed(ctx, byKey)
	return opened + o, failed + f, err
}

func itemKey(database string, queueID int) string {
	return fmt.Sprintf("%s\x00%d", database, queueID)
}

// claim inserts the change's row; false when it already has one.
func (p *ApprovalPRs) claim(ctx context.Context, item ApprovalItem) (ApprovalPRRecord,
	bool, error) {
	path, err := p.Config.filePath(item.Card.Database, item.Card.QueueID)
	if err != nil {
		slog.Warn("approval PR not opened: no valid file path", "database",
			item.Card.Database, "queue_id", item.Card.QueueID, "error", err)
		return ApprovalPRRecord{}, false, nil
	}
	return claimApprovalPR(ctx, p.Pool, ApprovalPRRecord{Database: item.Card.Database,
		QueueID: item.Card.QueueID, Repo: p.Config.Repo, Path: path,
		CardHash: item.Card.CardHash, ExpiresAt: p.expiry(item.Card)})
}

// retryFailed moves each failed open whose backoff passed back to opening
// and opens it again, while its change is still pending and allowed.
func (p *ApprovalPRs) retryFailed(ctx context.Context, pending map[string]ApprovalItem,
) (opened, failed int, err error) {
	rows, err := listApprovalPRs(ctx, p.Pool, PRStateOpenFailed)
	if err != nil {
		return 0, 0, err
	}
	for _, rec := range rows {
		item, ok := pending[itemKey(rec.Database, rec.QueueID)]
		if !ok || rec.NextAttemptAt == nil || rec.NextAttemptAt.After(p.now()) {
			continue
		}
		if allowed, _ := p.remoteAllowed(item); !allowed {
			continue
		}
		moved, err := transition{ID: rec.ID, From: PRStateOpenFailed, To: PRStateOpening,
			Detail: rec.Detail}.apply(ctx, p.Pool)
		if err != nil || !moved {
			return opened, failed, err
		}
		o, f := p.openOne(ctx, rec, item)
		opened, failed = opened+o, failed+f
		if p.Disabled() != "" {
			break
		}
	}
	return opened, failed, nil
}

// openOne opens the PR of a claimed row and records the outcome.
func (p *ApprovalPRs) openOne(ctx context.Context, rec ApprovalPRRecord,
	item ApprovalItem) (opened, failed int) {
	content, err := renderApprovalFile(item, rec.ExpiresAt)
	if err == nil {
		var pr codehost.PullRequest
		pr, err = p.Host.OpenPR(ctx, p.Config.Repo, p.prRequest(rec, item, content))
		if err == nil {
			_, err = transition{ID: rec.ID, From: PRStateOpening, To: PRStateOpen,
				PRNumber: pr.Number, PRURL: pr.URL, FileHash: fileHash(content),
				CardHash: item.Card.CardHash, Opened: true}.apply(ctx, p.Pool)
			if err != nil {
				slog.Error("approval PR opened but not recorded", "database", rec.Database,
					"queue_id", rec.QueueID, "pr", pr.Number, "error", err)
			}
			return 1, 0
		}
	}
	status := openFailureStatus(err)
	if errors.Is(err, codehost.ErrForbidden) {
		p.disable(err)
	}
	next := p.now().Add(retryBackoff(rec.Attempts + 1))
	if _, terr := (transition{ID: rec.ID, From: PRStateOpening, To: PRStateOpenFailed,
		Detail: "PR not opened: " + status, NextAttempt: &next,
		Failed: true}).apply(ctx, p.Pool); terr != nil {
		slog.Error("approval PR failure not recorded", "database", rec.Database,
			"queue_id", rec.QueueID, "error", terr)
	}
	slog.Warn("approval PR not opened", "database", rec.Database, "queue_id", rec.QueueID,
		"status", status, "retry_at", next)
	return 0, 1
}

// prRequest is the PR of one change: one branch, one file, the label.
func (p *ApprovalPRs) prRequest(rec ApprovalPRRecord, item ApprovalItem,
	content string) codehost.PRRequest {
	c := item.Card
	title := fmt.Sprintf("pg_sage approval: %s on %s (#%d)", oneLine(c.Title), c.Database,
		c.QueueID)
	body := fmt.Sprintf("pg_sage asks to approve change %d on %s (risk tier %s).\n\n"+
		"```sql\n%s\n```\n\nMerging this pull request approves and runs exactly this "+
		"change, as the merger's pg_sage user (mapped in principals/users.yaml, with the "+
		"operator role on %s). Do not edit the file: an edited file is refused. Closing "+
		"the pull request rejects the change. It closes as expired at %s.\n",
		c.QueueID, c.Database, item.Tier, c.SQL, c.Database,
		rec.ExpiresAt.UTC().Format("2006-01-02 15:04 MST"))
	return codehost.PRRequest{Title: title, Body: body,
		Branch: fmt.Sprintf("pg_sage/approval-%d", rec.ID), Base: p.Config.BaseBranch,
		CommitMessage: title, Labels: []string{ApprovalPRLabel},
		Files: []codehost.FileChange{{Path: rec.Path, Content: content}}}
}

// oneLine keeps a title to one line of at most 120 characters.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		s = string(r[:117]) + "..."
	}
	if s == "" {
		return "change"
	}
	return s
}
