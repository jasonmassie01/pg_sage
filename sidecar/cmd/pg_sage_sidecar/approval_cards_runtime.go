package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Approval cards (roadmap 1.5): wiring.
//
// Every executor's approval request goes through an approval-card notifier
// for its database; the shared dispatcher of the notification control pool
// mints the card tokens there (where the chat callbacks look them up); a
// loop posts verification verdicts back to the chats and re-sends cards
// whose snooze ended.

// approvalCardInterval is how often follow-ups and snoozes are checked.
const approvalCardInterval = 30 * time.Second

// withApprovalCardTokens makes a control pool's dispatcher mint card tokens.
func withApprovalCardTokens(d *notify.Dispatcher, controlPool *pgxpool.Pool) {
	if d == nil || controlPool == nil {
		return
	}
	d.WithCardTokens(approvalcard.TokenIssuer{Store: chatops.NewCardStore(controlPool)})
}

// executorDispatcher is the dispatcher an executor sends through: the
// shared dispatcher wrapped in its database's approval-card notifier.
func executorDispatcher(d *notify.Dispatcher, pool *pgxpool.Pool, name string,
	ex *executor.Executor) executor.EventDispatcher {
	if d == nil {
		return nil
	}
	l := approvalCardLoader(pool, name, "")
	return approvalcard.NewNotifier(d, l).WithLog(logStructuredWrapper).
		WithTrust(ex.TrustLevel)
}

// approvalCardLoader reads a database's cards with the executor's trust
// level and each card's class trust from the process's trust ledgers.
func approvalCardLoader(pool *pgxpool.Pool, name, trustLevel string) approvalcard.Loader {
	return approvalcard.Loader{Pool: pool, Database: name, TrustLevel: trustLevel,
		Trust: processAutonomy().registry}
}

// startApprovalCardLoop runs the follow-up and snooze loop until ctx ends.
func startApprovalCardLoop(ctx context.Context, controlPool *pgxpool.Pool,
	mgr *fleet.DatabaseManager) {
	d := sharedNotifyDispatcher(controlPool)
	if d == nil || mgr == nil {
		return
	}
	followups := &approvalcard.Followups{Store: chatops.NewCardStore(controlPool),
		Sender: d, Pools: func(name string) *pgxpool.Pool {
			if inst := mgr.GetInstance(name); inst != nil {
				return inst.Pool
			}
			return nil
		}}
	go func() {
		ticker := time.NewTicker(approvalCardInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runApprovalCardCycle(ctx, followups, d, mgr)
			}
		}
	}()
}

// runApprovalCardCycle posts due follow-ups and re-sends ended snoozes.
func runApprovalCardCycle(ctx context.Context, f *approvalcard.Followups,
	d *notify.Dispatcher, mgr *fleet.DatabaseManager) {
	if n, err := f.RunOnce(ctx); err != nil {
		logWarn("approvals", "approval follow-ups: %d posted, errors: %v", n, err)
	}
	for _, inst := range mgr.Instances() {
		if inst == nil || inst.Pool == nil || inst.Executor == nil {
			continue
		}
		l := approvalCardLoader(inst.Pool, inst.Name, inst.Executor.TrustLevel())
		n := approvalcard.NewNotifier(d, l).WithLog(logStructuredWrapper)
		if _, err := approvalcard.RenotifySnoozed(ctx, l, n); err != nil {
			logWarn("approvals", "db %q: re-send snoozed approval cards: %v", inst.Name, err)
		}
	}
}
