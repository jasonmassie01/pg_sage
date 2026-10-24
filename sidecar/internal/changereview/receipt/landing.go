package receipt

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/impact"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
)

// anchorKinds are the change events that say a PR's change ran: signed
// deploy and migration events, and DDL pg_sage saw. Their link must be the
// review's PR.
var anchorKinds = map[changefeed.Kind]bool{changefeed.KindDeploy: true,
	changefeed.KindMigration: true, changefeed.KindDDL: true}

// landing is the per-cycle context of one database's landing pass.
type landing struct {
	st     *changereview.Store
	reader catalogread.Reader
	now    time.Time
	// observed is when the post-conditions were read: the upper end of a
	// catalog landing (now, the cycle start, is the lower end recorded for
	// the reviews still unmet).
	observed time.Time
	expiry   time.Duration
	anchors  map[string]changefeed.Event
	feedOK   bool
	checks   map[string]*time.Time
	// states is each review's post-condition landing (impact.Landing);
	// failed holds the reviews whose evaluation failed, with the error.
	states map[string]string
	failed map[string]error
}

// land evaluates the awaiting reviews checked longest ago (never checked
// first): anchors first, then catalog post-conditions, then expiry.
// Reviews still unmet are recorded as checked, the lower end of a later
// catalog landing, and wait behind the others for the next cycle.
func (j *Job) land(ctx context.Context, db Database, now time.Time, rep *Report) error {
	st := db.Service.Store()
	awaiting, err := st.ListAwaiting(ctx, MaxAwaitingPerCycle)
	if err != nil {
		return fmt.Errorf("list awaiting reviews: %w", err)
	}
	rep.Evaluated = len(awaiting)
	if len(awaiting) == 0 {
		return nil
	}
	l := landing{st: st, reader: catalogread.New(db.Pool, j.d.Timeouts), now: now,
		expiry: time.Duration(db.Service.Config().LandingExpiryDays) * 24 * time.Hour,
		feedOK: true}
	if l.anchors, err = anchorsOf(ctx, db, awaiting, now); err != nil {
		rep.addError("change feed: landing anchors unavailable, expiry postponed: %v", err)
		l.feedOK = false
	}
	if l.checks, err = readChecks(ctx, db.Pool, awaiting); err != nil {
		return fmt.Errorf("read landing checks: %w", err)
	}
	l.states, l.failed = evaluateAll(ctx, l.reader, awaiting)
	l.observed = j.d.Now()
	var unmet []string
	for _, r := range awaiting {
		if err := ctx.Err(); err != nil {
			return err
		}
		if j.landOne(ctx, &l, r, rep) {
			unmet = append(unmet, r.ID)
		}
	}
	if err := recordChecks(ctx, db.Pool, unmet, now); err != nil {
		rep.addError("record landing checks: %v", err)
	}
	return nil
}

// landOne lands, marks not applicable or expires r; it reports whether r
// has post-conditions that are still unmet.
func (j *Job) landOne(ctx context.Context, l *landing, r changereview.Review,
	rep *Report) bool {
	if e, ok := l.anchors[r.ID]; ok {
		j.record(rep, r.ID, "landing", func() error {
			_, err := l.st.MarkLanded(ctx, r.ID, changereview.Landing{
				Source: changereview.LandingSourceChangeEvent, Lower: e.OccurredAt,
				At: e.OccurredAt})
			return err
		}, &rep.Landed)
		return false
	}
	unmet, known := false, true
	if len(r.Expectations.PostConditions) > 0 {
		switch err := l.failed[r.ID]; {
		case err != nil:
			rep.addError("review %s: evaluate post-conditions: %v", r.ID, err)
			known = false
		case l.states[r.ID] == impact.PostLanded:
			j.landByCatalog(ctx, l, r, rep)
			return false
		default:
			unmet = true
		}
	}
	if known && l.feedOK && r.CompletedAt != nil && !l.now.Before(r.CompletedAt.Add(l.expiry)) {
		j.record(rep, r.ID, "expiry", func() error {
			_, err := l.st.Expire(ctx, r.ID)
			return err
		}, &rep.Expired)
		return false
	}
	return unmet
}

// landByCatalog lands r whose post-conditions hold: between the last
// check that saw them unmet (the review's own at completion, then each
// cycle's) and now. A review never seen unmet (its check at review time
// failed, or it predates that check) is bracketed from its creation: a
// reviewed change is presumed to land after its review. A change that was
// there before the review is not applicable from completion, never here.
func (j *Job) landByCatalog(ctx context.Context, l *landing, r changereview.Review,
	rep *Report) {
	lower := r.CreatedAt
	if checked := l.checks[r.ID]; checked != nil {
		lower = *checked
	}
	if lower.After(l.observed) {
		lower = l.observed
	}
	j.record(rep, r.ID, "landing", func() error {
		_, err := l.st.MarkLanded(ctx, r.ID, changereview.Landing{
			Source: changereview.LandingSourceCatalog, Lower: lower, At: l.observed})
		return err
	}, &rep.Landed)
}

// anchorsOf maps awaiting reviews to the change event that landed them.
func anchorsOf(ctx context.Context, db Database, awaiting []changereview.Review,
	now time.Time) (map[string]changefeed.Event, error) {
	byPR := map[string][]changereview.Review{}
	since := now
	for _, r := range awaiting {
		if r.PRURL != "" {
			byPR[r.PRURL] = append(byPR[r.PRURL], r)
			if r.CreatedAt.Before(since) {
				since = r.CreatedAt
			}
		}
	}
	if len(byPR) == 0 || db.Changes == nil {
		return nil, nil
	}
	events, err := listEvents(ctx, db.Changes, since, now.Add(5*time.Minute))
	if err != nil {
		return nil, err
	}
	byLink := anchorEvents(events)
	out := map[string]changefeed.Event{}
	for pr, revs := range byPR {
		evs := byLink[pr]
		if len(evs) == 0 {
			continue
		}
		complete, err := db.Service.Store().List(ctx, changereview.ListFilter{PRURL: pr,
			State: changereview.StateComplete, Limit: changereview.MaxListLimit})
		if err != nil {
			return nil, err
		}
		for _, r := range revs {
			if e, ok := anchorFor(r, evs, complete); ok {
				out[r.ID] = e
			}
		}
	}
	return out, nil
}

// anchorEvents groups the anchoring events by link, oldest first.
func anchorEvents(events []changefeed.Event) map[string][]changefeed.Event {
	out := map[string][]changefeed.Event{}
	for _, e := range events {
		signed := e.Signature == changefeed.SignatureVerified ||
			e.Signature == changefeed.SignatureInternal
		if anchorKinds[e.Kind] && signed && e.Link != "" {
			out[e.Link] = append(out[e.Link], e)
		}
	}
	for _, evs := range out {
		sort.Slice(evs, func(a, b int) bool {
			return evs[a].OccurredAt.Before(evs[b].OccurredAt)
		})
	}
	return out
}

// anchorFor is the first event after r was created that shipped r: the
// newest complete review of the PR created before the event is r, or has
// r's content.
func anchorFor(r changereview.Review, events []changefeed.Event,
	complete []changereview.Review) (changefeed.Event, bool) {
	for _, e := range events {
		if e.OccurredAt.Before(r.CreatedAt) {
			continue
		}
		for _, c := range complete {
			if c.CreatedAt.After(e.OccurredAt) {
				continue
			}
			if c.ID == r.ID || (r.ContentHash != "" && c.ContentHash == r.ContentHash) {
				return e, true
			}
			break
		}
	}
	return changefeed.Event{}, false
}

const readChecksSQL = `/* pg_sage change_review_receipt_checks v1 */
SELECT id::text, landing_checked_at FROM sage.change_review WHERE id = ANY($1::uuid[])`

// readChecks reads the awaiting reviews' last unmet checks.
func readChecks(ctx context.Context, pool *pgxpool.Pool,
	reviews []changereview.Review) (map[string]*time.Time, error) {
	ids := make([]string, 0, len(reviews))
	for _, r := range reviews {
		ids = append(ids, r.ID)
	}
	rows, err := pool.Query(ctx, readChecksSQL, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*time.Time{}
	for rows.Next() {
		var id string
		var at *time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

const recordChecksSQL = `/* pg_sage change_review_receipt_checked v1 */
UPDATE sage.change_review SET landing_checked_at = $2
WHERE id = ANY($1::uuid[]) AND landing_state = 'awaiting'`

// recordChecks stamps the reviews seen unmet at now. It leaves updated_at
// (the retention clock) alone.
func recordChecks(ctx context.Context, pool *pgxpool.Pool, ids []string,
	now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := pool.Exec(ctx, recordChecksSQL, ids, now.UTC().Truncate(time.Microsecond))
	return err
}
