package approvalcard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/notify"
)

// endedSnooze is a pending queue item whose snooze ran out.
type endedSnooze struct {
	id               int
	sql, risk, title string
}

// RenotifySnoozed sends the card of every item whose snooze ran out again,
// once per snooze. Each item is claimed for five minutes before its card
// goes out, so racing runs send it once; a sent item's snooze ends, a
// failed one is retried when the claim runs out. It returns how many cards
// were sent and an error naming each item that was not.
func RenotifySnoozed(ctx context.Context, l Loader, d Dispatcher) (int, error) {
	if l.Pool == nil || noDispatcher(d) {
		return 0, fmt.Errorf("approvalcard: re-notify needs a pool and a dispatcher")
	}
	items, err := listEndedSnoozes(ctx, l.Pool)
	if err != nil {
		return 0, err
	}
	var errs []error
	sent := 0
	for _, e := range items {
		ok, err := resendSnoozed(ctx, l, d, e)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			sent++
		}
	}
	return sent, errors.Join(errs...)
}

// noDispatcher reports a missing dispatcher, including a nil Notifier
// (notifications off), whose Dispatch sends nothing.
func noDispatcher(d Dispatcher) bool {
	n, isNotifier := d.(*Notifier)
	return d == nil || (isNotifier && n == nil)
}

// listEndedSnoozes lists the pending, unexpired items whose snooze ran out.
func listEndedSnoozes(ctx context.Context, pool *pgxpool.Pool) ([]endedSnooze, error) {
	rows, err := pool.Query(ctx, `/* pg_sage */ SELECT q.id, q.proposed_sql, q.action_risk,
		(SELECT COALESCE(f.title, '') FROM sage.findings f WHERE f.id = q.finding_id)
		FROM sage.action_queue q
		WHERE q.snoozed_until IS NOT NULL AND q.snoozed_until <= now()
		  AND q.status = 'pending' AND q.expires_at > now()
		ORDER BY q.snoozed_until, q.id`)
	if err != nil {
		return nil, fmt.Errorf("approvalcard: list ended snoozes: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (endedSnooze, error) {
		var e endedSnooze
		var title *string
		err := r.Scan(&e.id, &e.sql, &e.risk, &title)
		if title != nil {
			e.title = *title
		}
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("approvalcard: list ended snoozes: %w", err)
	}
	return items, nil
}

// resendSnoozed claims one ended snooze, sends its card and ends the
// snooze. It reports false with no error when another run claimed it.
func resendSnoozed(ctx context.Context, l Loader, d Dispatcher, e endedSnooze) (bool, error) {
	var claim time.Time
	err := l.Pool.QueryRow(ctx, `/* pg_sage */ UPDATE sage.action_queue
		SET snoozed_until = now() + interval '5 minutes'
		WHERE id = $1 AND snoozed_until <= now()
		  AND status = 'pending' AND expires_at > now()
		RETURNING snoozed_until`, e.id).Scan(&claim)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("approvalcard: claim snoozed card %d: %w", e.id, err)
	}
	evt := notify.QueuedApprovalEvent(e.title, e.sql, l.Database, e.risk, e.id)
	if err := d.Dispatch(ctx, evt); err != nil {
		return false, fmt.Errorf("approvalcard: re-send card %d: %w (retrying in 5 minutes)",
			e.id, err)
	}
	// The claim's own value: a snooze set again meanwhile stays.
	if _, err := l.Pool.Exec(ctx, `/* pg_sage */ UPDATE sage.action_queue
		SET snoozed_until = NULL WHERE id = $1 AND snoozed_until = $2`,
		e.id, claim); err != nil {
		return true, fmt.Errorf("approvalcard: end snooze of re-sent card %d: %w", e.id, err)
	}
	return true, nil
}
