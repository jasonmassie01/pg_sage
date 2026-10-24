package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Change Review's DBA notification: a review that completes with verdict
// review or risky (and no carried-over DBA verdict) is announced once as
// change_review_needs_dba through the shared notify dispatcher, so the
// operator's rules and channels decide who hears about it. The service
// calls ReviewNeedsDBA once per such review (changereview.Notifier).

const changeReviewNotifyTimeout = 30 * time.Second

// eventDispatcher is the part of notify.Dispatcher the notifier uses.
type eventDispatcher interface {
	Dispatch(ctx context.Context, event notify.Event) error
}

type changeReviewNotifier struct {
	dispatcher eventDispatcher
	render     changereview.RenderOptions
	logf       func(format string, args ...any)
}

// newChangeReviewNotifier is the service's Notifier over d, linking each
// review in the UI through render; nil (no notifications) without a
// dispatcher. The database runtime passes rt.dispatcher.
func newChangeReviewNotifier(d *notify.Dispatcher, render changereview.RenderOptions,
	logf func(format string, args ...any)) changereview.Notifier {
	if d == nil {
		return nil
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return changeReviewNotifier{dispatcher: d, render: render, logf: logf}
}

// ReviewNeedsDBA dispatches in the background so a slow channel never
// holds the review runner's slot.
func (n changeReviewNotifier) ReviewNeedsDBA(ctx context.Context, r changereview.Review) {
	event := notify.ChangeReviewNeedsDBAEvent(changeReviewInfo(r, n.render))
	go func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
			changeReviewNotifyTimeout)
		defer cancel()
		if err := n.dispatcher.Dispatch(dctx, event); err != nil {
			n.logf("change review: %s: notify %s for review %s: %v", r.Database,
				notify.EventChangeReviewNeedsDBA, r.ID, notify.RedactError(err))
		}
	}()
}

// changeReviewInfo is the notification payload of r: its most severe
// finding (the first critical, else the first warning) leads.
func changeReviewInfo(r changereview.Review,
	o changereview.RenderOptions) notify.ChangeReviewInfo {
	info := notify.ChangeReviewInfo{ID: r.ID, Database: r.Database, PRURL: r.PRURL,
		PRTitle: r.PRTitle, Verdict: r.Verdict, Critical: r.Summary.Critical,
		Warning: r.Summary.Warning, Link: changereview.ReviewLink(r, o)}
	rank := map[string]int{changereview.SeverityCritical: 2, changereview.SeverityWarning: 1}
	best := -1
	for _, f := range r.Findings {
		if rank[f.Severity] > best && rank[f.Severity] > 0 {
			best = rank[f.Severity]
			info.TopRule, info.TopFinding = f.RuleID, f.Title
		}
	}
	return info
}
