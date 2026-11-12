package cases

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The sync worker's bounds: one cycle every 15 minutes, at most 50
// tickets, each ticket at most once per cycle.
const (
	TicketSyncInterval = 15 * time.Minute
	TicketSyncLimit    = 50
)

// CaseStateFunc reads a case's state; known is false when its database
// could not be read. A case its database no longer reports is resolved.
type CaseStateFunc func(ctx context.Context, database, identityKey string) (State, bool)

// TicketSyncer reads ticket status back and comments once on a ticket
// whose case is no longer open. It reads the case state, never writes it.
type TicketSyncer struct {
	Store     TicketStore
	Tracker   TrackerFunc
	CaseState CaseStateFunc
	Limit     int                  // 0 = TicketSyncLimit
	Log       func(string, ...any) // nil discards
}

// TicketSyncReport counts one cycle.
type TicketSyncReport struct {
	Checked, Updated, Failed, Commented int
}

// Run syncs once at start and then every interval until ctx ends.
func (s *TicketSyncer) Run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if _, err := s.SyncOnce(ctx); err != nil {
			s.logf("case ticket sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// SyncOnce syncs the tickets that are due.
func (s *TicketSyncer) SyncOnce(ctx context.Context) (TicketSyncReport, error) {
	limit := s.Limit
	if limit <= 0 {
		limit = TicketSyncLimit
	}
	due, err := s.Store.DueForSync(ctx, limit, TicketSyncInterval/2)
	if err != nil {
		return TicketSyncReport{}, fmt.Errorf("listing tickets due for sync: %w", err)
	}
	var rep TicketSyncReport
	for _, t := range due {
		rep.Checked++
		s.syncTicket(ctx, t, &rep)
	}
	return rep, nil
}

func (s *TicketSyncer) syncTicket(ctx context.Context, t Ticket, rep *TicketSyncReport) {
	st, err := s.readStatus(ctx, t)
	if err != nil {
		rep.Failed++
		class := ticketSyncError(err)
		s.logf("case ticket %s (%s): sync failed: %s", t.ExternalKey, t.Database, class)
		if rerr := s.Store.RecordSync(ctx, t.ID, IssueStatus{}, class); rerr != nil {
			s.logf("case ticket %s: recording sync failure: %v", t.ExternalKey, rerr)
		}
		return
	}
	if err := s.Store.RecordSync(ctx, t.ID, st, ""); err != nil {
		rep.Failed++
		s.logf("case ticket %s: recording status: %v", t.ExternalKey, err)
		return
	}
	rep.Updated++
	if s.commentIfResolved(ctx, t) {
		rep.Commented++
	}
}

func (s *TicketSyncer) readStatus(ctx context.Context, t Ticket) (IssueStatus, error) {
	if t.ChannelID == nil {
		return IssueStatus{}, ErrTicketIntegrationMissing
	}
	tracker, err := s.Tracker(ctx, *t.ChannelID)
	if err != nil {
		return IssueStatus{}, err
	}
	return tracker.IssueStatus(ctx, t.ExternalKey)
}

// commentIfResolved adds the one resolve comment when the case is known
// to be no longer open. The comment is claimed first, so two sidecars
// sharing the control database comment once; a failed comment is
// released for the next cycle.
func (s *TicketSyncer) commentIfResolved(ctx context.Context, t Ticket) bool {
	if t.ResolvedCommentAt != nil || s.CaseState == nil {
		return false
	}
	state, known := s.CaseState(ctx, t.Database, t.CaseIdentityKey)
	if !known || state == StateOpen {
		return false
	}
	claimed, err := s.Store.ClaimResolvedComment(ctx, t.ID)
	if err != nil || !claimed {
		return false
	}
	tracker, err := s.Tracker(ctx, *t.ChannelID)
	if err == nil {
		err = tracker.AddComment(ctx, t.ExternalKey, fmt.Sprintf("pg_sage: case %s on %s is "+
			"no longer open (%s). This comment is the only change pg_sage makes to the issue.",
			t.CaseIdentityKey, t.Database, state))
	}
	if err != nil {
		s.logf("case ticket %s: resolve comment: %s", t.ExternalKey, ticketSyncError(err))
		if rerr := s.Store.ReleaseResolvedComment(ctx, t.ID); rerr != nil {
			s.logf("case ticket %s: releasing the comment claim: %v", t.ExternalKey, rerr)
		}
		return false
	}
	return true
}

// ticketSyncError is the class a failed sync shows on the case.
func ticketSyncError(err error) string {
	switch {
	case errors.Is(err, ErrTicketUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrTicketNotFound):
		return "not_found"
	case errors.Is(err, ErrTicketIntegrationMissing):
		return "integration_missing"
	default:
		return "unreachable"
	}
}

func (s *TicketSyncer) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}
