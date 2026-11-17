package briefing

import (
	"context"
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/outcomes"
)

// Gathering the digest (ux-02b): the window's outcome ledger rows and the
// open held-back decisions are read from the database's pg_sage schema,
// and the inbox counts from the counter the sidecar wires (ux-01a). A
// source that cannot be read is a gap in the digest, never a zero.

// digestSourceTimeout bounds each source read.
const digestSourceTimeout = 10 * time.Second

// InboxCounter counts what waits for a person on one database.
type InboxCounter func(ctx context.Context, database string) (InboxCounts, error)

// WithInboxCounter makes the digest's Needs you section read f.
func (w *Worker) WithInboxCounter(f InboxCounter) *Worker {
	w.inbox = f
	return w
}

// buildDigest gathers and builds the digest of win; askModel lets the
// configured model word its headline.
func (w *Worker) buildDigest(ctx context.Context, win Window, askModel bool) *Digest {
	in := w.gatherDigest(ctx, win)
	d := BuildDigest(in)
	if askModel {
		w.digestHeadline(ctx, &d)
	} else if w.llm != nil && w.llm.IsEnabled() {
		d.HeadlineFallback = "the model failed on this run's briefing"
	}
	return &d
}

func (w *Worker) gatherDigest(ctx context.Context, win Window) DigestInput {
	in := DigestInput{Database: w.database, Window: win,
		DBAHourRateUSD: w.settings().cfg.DBAHourRateUSD}
	if w.pool == nil {
		err := errors.New("no database pool")
		in.OutcomesErr, in.HeldBackErr = err, err
	} else {
		in.Outcomes, in.OutcomesErr = w.readOutcomes(ctx, win)
		in.HeldBack, in.HeldBackErr = w.readHeldBack(ctx)
	}
	in.Inbox, in.InboxErr = w.readInbox(ctx)
	return in
}

// ledgerSince is win's span as a ledger window, within its bounds.
func ledgerSince(win Window) time.Duration {
	since := win.End.Sub(win.Start)
	switch {
	case since < time.Second:
		return time.Second
	case since > outcomes.MaxSince:
		return outcomes.MaxSince
	}
	return since
}

func (w *Worker) readOutcomes(ctx context.Context, win Window) (outcomes.Result, error) {
	rctx, cancel := context.WithTimeout(ctx, digestSourceTimeout)
	defer cancel()
	res, err := outcomes.Read(rctx, w.pool, w.database, outcomes.Query{
		Since: ledgerSince(win), Now: win.End, Limit: outcomes.MaxLimit})
	if err != nil {
		w.logFn("WARN", "briefing: digest: %v", err)
	}
	return res, err
}

func (w *Worker) readHeldBack(ctx context.Context) ([]ledger.WithheldEntry, error) {
	rctx, cancel := context.WithTimeout(ctx, digestSourceTimeout)
	defer cancel()
	entries, _, err := ledger.ListOpenWithheld(rctx, w.pool,
		ledger.WithheldFilter{Limit: ledger.MaxWithheldLimit})
	if err != nil {
		w.logFn("WARN", "briefing: [%s] digest: read held-back decisions: %v",
			w.database, err)
	}
	return entries, err
}

func (w *Worker) readInbox(ctx context.Context) (*InboxCounts, error) {
	if w.inbox == nil {
		return nil, errors.New("no inbox counter is wired")
	}
	rctx, cancel := context.WithTimeout(ctx, digestSourceTimeout)
	defer cancel()
	counts, err := w.inbox(rctx, w.database)
	if err != nil {
		w.logFn("WARN", "briefing: [%s] digest: count the inbox: %v", w.database, err)
		return nil, err
	}
	return &counts, nil
}
