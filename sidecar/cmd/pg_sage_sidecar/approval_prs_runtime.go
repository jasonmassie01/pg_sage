package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/gitops"
)

// Approval requests as pull requests (cli-02b): with policy.approval_prs
// set, the leader polls the config repository every approvalPRPoll,
// opening a PR per change waiting for approval and deciding merged and
// closed ones. A follower does nothing (one replica writes). The settings
// take effect at restart.

const approvalPRPoll = time.Minute

// newApprovalPRs builds the runner; nil when approval PRs are off.
func newApprovalPRs(c *config.Config, control *pgxpool.Pool, mgr *fleet.DatabaseManager,
) (*gitops.ApprovalPRs, error) {
	if c == nil || !c.Policy.ApprovalPRs.Enabled() || control == nil {
		return nil, nil
	}
	ap := c.Policy.ApprovalPRs
	getenv, err := secretGetenv(config.ApprovalPRsTokenEnv)
	if err != nil {
		return nil, fmt.Errorf("policy.approval_prs: read %s: %w", config.ApprovalPRsTokenEnv,
			err)
	}
	token := getenv(config.ApprovalPRsTokenEnv)
	if token == "" {
		return nil, fmt.Errorf("policy.approval_prs.repo is set but %s is empty",
			config.ApprovalPRsTokenEnv)
	}
	host, err := buildCodeHost("policy.approval_prs", ap.Provider, codehost.Options{
		BaseURL: ap.BaseURL, Token: token,
		Timeout: time.Duration(ap.TimeoutSeconds) * time.Second})
	if err != nil {
		return nil, err
	}
	return &gitops.ApprovalPRs{Host: host, Pool: control,
		Queue: &api.ApprovalPRQueue{Mgr: mgr},
		Users: &gitops.DirectoryUsers{Pool: control, Root: gitopsDirectory(c)},
		Config: gitops.ApprovalPRConfig{Repo: ap.Repo, BaseBranch: ap.BaseBranch,
			Path: ap.EffectivePath(), Expiry: ap.Expiry(), ChatCap: chatApprovalCap()}}, nil
}

// startApprovalPRs starts the leader's poll; a configuration that cannot
// run is logged and leaves approvals in the app and chat.
func startApprovalPRs(ctx context.Context, control *pgxpool.Pool,
	mgr *fleet.DatabaseManager) {
	prs, err := newApprovalPRs(cfg, control, mgr)
	if err != nil {
		logError("approval-prs", "approval PRs are off: %v", err)
		return
	}
	if prs == nil {
		return
	}
	api.SetApprovalPRs(prs, control)
	logInfo("approval-prs", "opening approval requests as pull requests on %s",
		prs.Config.Repo)
	go func() {
		t := time.NewTicker(approvalPRPoll)
		defer t.Stop()
		for {
			approvalPRTick(ctx, prs)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// approvalPRTick runs one poll on the leader.
func approvalPRTick(ctx context.Context, prs *gitops.ApprovalPRs) {
	if !currentFleetLeader().IsLeader() {
		return
	}
	wasOff := prs.Disabled() != ""
	res, err := prs.Sync(ctx)
	if err != nil {
		logWarn("approval-prs", "poll failed, retrying next poll: %v", err)
		return
	}
	if reason := prs.Disabled(); reason != "" && !wasOff {
		logError("approval-prs", "%s", reason)
	}
	if res.Opened+res.Failed+res.Decided > 0 {
		logInfo("approval-prs", "opened %d, failed %d, decided %d", res.Opened, res.Failed,
			res.Decided)
	}
}
