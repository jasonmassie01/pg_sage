package main

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/notify"
)

// The digest's wiring (ux-02b): each database's briefing worker sends its
// digest through the shared Notifications dispatcher of its control
// database, and counts its Needs you section from the inbox (ux-01a).

// digestNotifier is the shared dispatcher with the store its rules live in.
type digestNotifier struct {
	d *notify.Dispatcher
	s *notify.PoolStore
}

func (n digestNotifier) SubmitWithOutcomes(ctx context.Context, e notify.Event) (
	notify.DispatchResult, []notify.ChannelOutcome, error,
) {
	return n.d.SubmitWithOutcomes(ctx, e)
}

func (n digestNotifier) EnsureDefaultRule(ctx context.Context, event string) (bool, error) {
	return n.s.EnsureDefaultRule(ctx, event)
}

// newBriefingWorker is the database's briefing worker: its digest goes
// through Notifications and its Needs you section counts the inbox.
func (rt *databaseRuntime) newBriefingWorker() *briefing.Worker {
	return briefing.New(rt.spec.Pool, cfg, rt.generalLLM, logStructuredWrapper).
		WithDatabaseName(rt.spec.Name).WithNotifier(rt.briefingNotifier()).
		WithInboxCounter(digestInboxCounts)
}

// briefingNotifier is the database runtime's digest notifier, or nil when
// it has no dispatcher (the digest then goes to stdout only, and its row
// says Notifications was not wired).
func (rt *databaseRuntime) briefingNotifier() briefing.Notifier {
	if rt.dispatcher == nil {
		return nil
	}
	store := sharedNotifyStore(rt.spec.ControlPool)
	if store == nil {
		return nil
	}
	return digestNotifier{d: rt.dispatcher, s: store}
}

// digestInboxCounts counts one database's inbox, as GET /api/v1/inbox
// does. A database no source could read is an error (a gap in the
// digest), not "nothing needs you".
func digestInboxCounts(ctx context.Context, database string) (briefing.InboxCounts, error) {
	var in inbox.Instances
	if fleetMgr != nil {
		in = fleetMgr
	}
	svc := &inbox.Service{Sources: inbox.FleetSources(in, processAutonomy().registry)}
	res := svc.List(ctx, inbox.Request{Databases: []string{database}, Limit: 1})
	if len(res.Errors) >= len(inbox.Kinds()) && res.Total == 0 {
		return briefing.InboxCounts{}, fmt.Errorf("no inbox source could be read for %q: %s",
			database, res.Errors[0].Error)
	}
	by := make(map[string]int, len(res.CountsByKind))
	for k, n := range res.CountsByKind {
		by[string(k)] = n
	}
	return briefing.InboxCounts{Total: res.Total, ByKind: by, LowerBound: res.LowerBound}, nil
}
