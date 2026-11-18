package gitops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Approval requests as pull requests (cli-02b). With policy.approval_prs
// set, each change waiting for approval whose tier a remote approval may
// decide (approvalcard.RemoteApprovalAllowed, decision D7) is opened as
// one pull request on the config repository, adding
// <path><database>/<queue_id>.yaml with the card hash, SQL, rollback, risk
// tier, ladder receipt and expiry. pg_sage polls the PR (no inbound
// webhook). A merge approves the change through the same single-use,
// SQL-matched approval as the app, as the merger's mapped pg_sage user,
// only while the merged file is the one pg_sage wrote and the card is
// unchanged; anything else is refused with a comment and the change stays
// pending in pg_sage. A PR closed unmerged rejects the change. A change
// decided elsewhere, or a PR past its expiry, is closed with a comment.

// Approval PR states (sage.approval_prs.state).
const (
	PRStateOpening    = "opening"
	PRStateOpen       = "open"
	PRStateOpenFailed = "open_failed"
	PRStateApproved   = "approved"
	PRStateRejected   = "rejected"
	PRStateRefused    = "refused"
	PRStateSuperseded = "superseded"
	PRStateExpired    = "expired"
)

const (
	// ApprovalPRLabel marks the pull requests pg_sage opens for approvals.
	ApprovalPRLabel = "pg_sage-approval"
	// AuditApprovalPR is the auth audit event of every state transition.
	AuditApprovalPR = "approval_pr_transition"
	// approvalFileKind is the kind of the file an approval PR adds.
	approvalFileKind = "ApprovalRequest"
)

var (
	// ErrAlreadyDecided is a change that is no longer waiting for approval.
	ErrAlreadyDecided = errors.New("gitops: the change was already decided")
	// ErrItemNotFound is a queue item that no longer exists.
	ErrItemNotFound = errors.New("gitops: the queued change no longer exists")
	// ErrUnmappedLogin is a code-host login with no active pg_sage user.
	ErrUnmappedLogin = errors.New("gitops: the code-host login is not mapped to a " +
		"pg_sage user")
	// ErrNotOperator is a mapped user who may not approve on the database.
	ErrNotOperator = errors.New("gitops: the user lacks the operator role on the database")
)

// ApprovalItem is one queued change as an approval card, with the risk
// tier a remote approval is capped by (the highest of the card's and the
// executor contracts', fail closed).
type ApprovalItem struct {
	Card approvalcard.Card
	Tier policy.RiskTier
}

// ApprovalQueue is the queue of changes across monitored databases and the
// app's own decision path; the API package implements it over the fleet.
type ApprovalQueue interface {
	// Pending lists the changes waiting for approval.
	Pending(ctx context.Context) ([]ApprovalItem, error)
	// Item reads one change in any status; ErrItemNotFound when it is gone.
	Item(ctx context.Context, database string, queueID int) (ApprovalItem, error)
	// Approve approves and runs the change as userID only while its SQL is
	// expectSQL; ErrAlreadyDecided when it is no longer pending.
	Approve(ctx context.Context, database string, queueID, userID int,
		expectSQL string) (string, error)
	// Reject rejects the change; ErrAlreadyDecided when it is not pending.
	Reject(ctx context.Context, database string, queueID, userID int, reason string) error
}

// Approver is the pg_sage user a code-host login is mapped to.
type Approver struct {
	UserID int
	Email  string
	Role   string
}

// ApprovalUsers maps a merger's login to a pg_sage user allowed to approve
// on database; ErrUnmappedLogin or ErrNotOperator otherwise.
type ApprovalUsers interface {
	Resolve(ctx context.Context, login, database string) (Approver, error)
}

// ApprovalPRConfig is policy.approval_prs plus the chat approval cap.
type ApprovalPRConfig struct {
	Repo       string
	BaseBranch string
	Path       string
	Expiry     time.Duration
	// ChatCap is notification_policy.chat_approval_max_tier.
	ChatCap string
}

// ApprovalPRs opens and decides approval PRs; Sync runs one poll.
type ApprovalPRs struct {
	Host   codehost.Host
	Pool   *pgxpool.Pool
	Queue  ApprovalQueue
	Users  ApprovalUsers
	Config ApprovalPRConfig
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	disabled string
}

// SyncResult counts what one poll did.
type SyncResult struct {
	Opened  int `json:"opened"`
	Failed  int `json:"failed"`
	Decided int `json:"decided"`
}

// ApprovalPRStatus is PR mode's state for the Settings banner.
type ApprovalPRStatus struct {
	Enabled  bool   `json:"enabled"`
	Repo     string `json:"repo,omitempty"`
	Disabled string `json:"disabled_reason,omitempty"`
}

// Status reports whether PR mode is on, and why it was turned off.
func (p *ApprovalPRs) Status() ApprovalPRStatus {
	if p == nil || strings.TrimSpace(p.Config.Repo) == "" {
		return ApprovalPRStatus{}
	}
	reason := p.Disabled()
	return ApprovalPRStatus{Enabled: reason == "", Repo: p.Config.Repo, Disabled: reason}
}

// Disabled is why PR mode turned itself off ("" while it is on).
func (p *ApprovalPRs) Disabled() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disabled
}

// disable turns PR mode off until restart: the token was refused.
func (p *ApprovalPRs) disable(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled == "" {
		p.disabled = fmt.Sprintf("the code host refused the token (%s); approval PRs are "+
			"off until %s is fixed and pg_sage restarts", openFailureStatus(err),
			"SAGE_APPROVAL_PRS_TOKEN")
	}
}

func (p *ApprovalPRs) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

// Sync runs one poll: it decides the open PRs, then opens a PR for each
// new change (and retries the failed ones whose backoff passed). Without
// a repo, or once disabled, it does nothing.
func (p *ApprovalPRs) Sync(ctx context.Context) (SyncResult, error) {
	var res SyncResult
	if p == nil || strings.TrimSpace(p.Config.Repo) == "" || p.Disabled() != "" {
		return res, nil
	}
	if p.Host == nil || p.Pool == nil || p.Queue == nil || p.Users == nil {
		return res, errors.New("gitops: approval PRs need a code host, the control " +
			"database, the queue and the user map")
	}
	decided, err := p.decideOpen(ctx)
	res.Decided = decided
	if err != nil {
		return res, err
	}
	if p.Disabled() != "" {
		return res, nil
	}
	opened, failed, err := p.openPending(ctx)
	res.Opened, res.Failed = opened, failed
	return res, err
}
