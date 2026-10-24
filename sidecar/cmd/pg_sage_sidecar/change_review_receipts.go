package main

import (
	"context"
	"sort"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/receipt"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Change Review receipts: the leader-only loop. Every 10 minutes the fleet
// leader runs one receipts cycle over every database with a registered
// Change Review service: landing, due receipts, shadow scores. A follower
// skips it (fleetLeaderAllows); the job keeps no state of its own, so a
// new leader picks up where the old one stopped.

const changeReviewReceiptsJob = "change review receipts"

// startChangeReviewReceipts runs the receipts loop until ctx ends.
func startChangeReviewReceipts(ctx context.Context, mgr *fleet.DatabaseManager) {
	if mgr == nil {
		return
	}
	timeouts := catalogread.Default()
	if cfg != nil {
		timeouts = catalogread.FromSafety(cfg.Safety)
	}
	job := receipt.NewJob(receipt.Deps{Timeouts: timeouts, Logf: func(f string, a ...any) {
		logWarn("change_review", f, a...)
	}})
	reg := changeReviewServices()
	go every(ctx, receipt.CycleInterval, func(c context.Context) {
		runChangeReviewReceipts(c, job, mgr, reg)
	})
}

// runChangeReviewReceipts runs one cycle when this sidecar leads and
// reports whether it ran.
func runChangeReviewReceipts(ctx context.Context, job *receipt.Job,
	mgr *fleet.DatabaseManager, reg *changereview.Registry) bool {
	if !fleetLeaderAllows(changeReviewReceiptsJob) {
		return false
	}
	for _, rep := range job.Cycle(ctx, changeReviewReceiptDatabases(mgr, reg)) {
		if len(rep.Errors) > 0 {
			logWarn("change_review", "receipts on %s: %d errors, first: %s", rep.Database,
				len(rep.Errors), rep.Errors[0])
		}
		if rep.Landed+rep.Expired+rep.Decided+rep.Scored > 0 {
			logInfo("change_review", "receipts on %s: %d landed, %d expired, %d decided, "+
				"%d scored", rep.Database, rep.Landed, rep.Expired, rep.Decided, rep.Scored)
		}
	}
	return true
}

// changeReviewReceiptDatabases are the fleet's databases that have a
// Change Review service, in name order.
func changeReviewReceiptDatabases(mgr *fleet.DatabaseManager,
	reg *changereview.Registry) []receipt.Database {
	if mgr == nil || reg == nil {
		return nil
	}
	var out []receipt.Database
	for name, inst := range mgr.Instances() {
		svc, ok := reg.Get(name)
		if !ok || inst == nil || inst.Pool == nil {
			continue
		}
		db := receipt.Database{Name: name, Service: svc, Pool: inst.Pool}
		if inst.Changes != nil { // never a typed nil in the interface
			db.Changes = inst.Changes
		}
		out = append(out, db)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}
