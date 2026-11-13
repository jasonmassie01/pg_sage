package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The cutover from the legacy alerting engine (alerting-06b). The legacy
// manager paged PagerDuty under <database>:<category>:<object> and logged
// each page in the monitored database's sage.alert_log; notify pages under
// its own key. A page opened before the cutover is still open there, so it
// is resolved under its legacy key, read from alert_log, once its finding
// resolves; new findings page under notify's key.

// legacyPagerDutyChannel is the channel name the legacy engine logged.
const legacyPagerDutyChannel = "pagerduty"

// maxLegacyResolveAttempts bounds failed resolves per finding, like the
// legacy engine, so a dead routing key is not retried forever.
const maxLegacyResolveAttempts = 5

// legacyPageBatch bounds one sweep; a var so a test can make it small.
var legacyPageBatch = 200

// openLegacyPagesSQL lists findings whose newest legacy PagerDuty trigger
// ('sent') no resolve followed, with the alert_log key and whether the
// finding has resolved since. A resolve or failed resolve logged before
// that trigger belongs to an earlier episode of a reopened finding and
// does not count. Resolved findings come first, so open ones never crowd
// a resolve out of the batch.
const openLegacyPagesSQL = `/* pg_sage */
SELECT finding_id, dedup_key, severity, title, category, object, resolved FROM (
    SELECT DISTINCT ON (a.finding_id) a.finding_id, a.dedup_key, a.sent_at, a.id,
           f.severity, f.title, f.category,
           COALESCE(f.object_identifier, '') AS object, f.status = 'resolved' AS resolved
      FROM sage.alert_log a
      JOIN sage.findings f ON f.id = a.finding_id
     WHERE a.channel = $1 AND a.status = 'sent'
     ORDER BY a.finding_id, a.sent_at DESC, a.id DESC
) t
 WHERE NOT EXISTS (SELECT 1 FROM sage.alert_log r
                    WHERE r.finding_id = t.finding_id AND r.channel = $1
                      AND r.status = 'resolved' AND (r.sent_at, r.id) > (t.sent_at, t.id))
   AND (SELECT count(*) FROM sage.alert_log e
         WHERE e.finding_id = t.finding_id AND e.channel = $1
           AND e.status = 'resolve_error' AND (e.sent_at, e.id) > (t.sent_at, t.id)) < $2
 ORDER BY resolved DESC, finding_id
 LIMIT $3`

const insertLegacyResolveSQL = `/* pg_sage */ INSERT INTO sage.alert_log
    (finding_id, severity, channel, dedup_key, status, error_message)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`

// LegacyCutoverResult counts one sweep: resolves sent, resolves that
// failed (retried next sweep) and legacy pages still owed a resolve.
type LegacyCutoverResult struct {
	Resolved  int
	Failed    int
	StillOpen int
	// More is set when the sweep read a full batch: pages beyond it are
	// read by the next sweep, so the cutover is not done yet.
	More bool
}

// LegacyPagerDutyKey is the dedup key the legacy engine paged with:
// <database>:<alert_log key> for a named database, bounded like any
// PagerDuty key.
func LegacyPagerDutyKey(database, alertLogKey string) string {
	key := alertLogKey
	if database != "" {
		key = database + ":" + key
	}
	return BoundPagerDutyDedupKey(key)
}

type legacyPage struct {
	findingID                         int64
	key, severity, title, cat, object string
	resolved                          bool
}

// ResolveLegacyPages resolves, through the reconciled PagerDuty channel
// ch, every legacy page on pool's database whose finding has resolved,
// and records each attempt in alert_log so a resolve goes out once.
func ResolveLegacyPages(
	ctx context.Context, pool *pgxpool.Pool, database string, d *Dispatcher, ch Channel,
) (LegacyCutoverResult, error) {
	var res LegacyCutoverResult
	if pool == nil || d == nil {
		return res, errors.New("legacy cutover: database pool or dispatcher is nil")
	}
	pages, err := openLegacyPages(ctx, pool)
	if err != nil {
		return res, err
	}
	res.More = len(pages) >= legacyPageBatch
	for _, p := range pages {
		if !p.resolved {
			res.StillOpen++
			continue
		}
		status, errMsg := "resolved", ""
		if err := d.SendDirect(ctx, ch, legacyResolveEvent(p, database)); err != nil {
			status, errMsg = "resolve_error", RedactError(err).Error()
			res.Failed++
			res.StillOpen++
		} else {
			res.Resolved++
		}
		if err := recordLegacyResolve(ctx, pool, p, status, errMsg); err != nil {
			return res, err
		}
	}
	return res, nil
}

func openLegacyPages(ctx context.Context, pool *pgxpool.Pool) ([]legacyPage, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := pool.Query(qctx, openLegacyPagesSQL, legacyPagerDutyChannel,
		maxLegacyResolveAttempts, legacyPageBatch)
	if err != nil {
		return nil, fmt.Errorf("legacy cutover: reading open pages from alert_log: %w", err)
	}
	pages, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (legacyPage, error) {
		var p legacyPage
		err := row.Scan(&p.findingID, &p.key, &p.severity, &p.title, &p.cat, &p.object,
			&p.resolved)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("legacy cutover: scanning open pages: %w", err)
	}
	return pages, nil
}

// legacyResolveEvent is the resolve of p under its legacy key.
func legacyResolveEvent(p legacyPage, database string) Event {
	return Event{Type: "finding_resolved", Severity: p.severity,
		Subject: "Resolved: " + p.title, Resolve: true,
		DedupKey: LegacyPagerDutyKey(database, p.key),
		Data: map[string]any{"database": database, "category": p.cat,
			"object": p.object, "legacy_cutover": true}}
}

func recordLegacyResolve(
	ctx context.Context, pool *pgxpool.Pool, p legacyPage, status, errMsg string,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(qctx, insertLegacyResolveSQL, p.findingID, p.severity,
		legacyPagerDutyChannel, p.key, status, errMsg); err != nil {
		return fmt.Errorf("legacy cutover: recording the %s of finding %d: %w",
			status, p.findingID, err)
	}
	return nil
}
