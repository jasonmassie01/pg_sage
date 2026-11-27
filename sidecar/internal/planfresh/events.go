package planfresh

import (
	"context"
	"time"
)

// eventsSQL reads, for the plan's relations only, their latest ANALYZE
// (manual or auto) and the latest executed, not failed, pg_sage action
// since the capture whose finding or decision names one of the plan's
// relations or indexes. A target matches on its unqualified name, so a
// same-named object in another schema also makes the plan stale (fail
// closed). Both reads are bounded by indexes: pg_stat_user_tables by name,
// sage.action_log by executed_at, sage.decision by action_log_id.
//
//	$1 "schema.relation" names, $2 unqualified relation names,
//	$3 lower-case object names, $4 the capture time.
const eventsSQL = `/* pg_sage */
SELECT
  (SELECT max(GREATEST(s.last_analyze, s.last_autoanalyze))
   FROM pg_stat_user_tables s
   WHERE s.schemaname || '.' || s.relname = ANY($1::text[])
      OR s.relname = ANY($2::text[])),
  (SELECT max(al.executed_at)
   FROM sage.action_log al
   LEFT JOIN sage.findings f ON f.id = al.finding_id
   WHERE al.executed_at >= $4 AND al.outcome <> 'failed'
     AND (lower(btrim(regexp_replace(COALESCE(f.object_identifier, ''), '^.*\.', ''), '"'))
            = ANY($3::text[])
          OR EXISTS (
            SELECT 1
            FROM sage.decision d
            CROSS JOIN LATERAL jsonb_array_elements_text(
              CASE jsonb_typeof(d.target_objects) WHEN 'array' THEN d.target_objects
                   ELSE '[]'::jsonb END) AS t(target)
            WHERE d.action_log_id = al.id
              AND lower(btrim(regexp_replace(t.target, '^.*\.', ''), '"')) = ANY($3::text[]))))`

func (r Reader) events(ctx context.Context, n Names, capturedAt time.Time) (Events, error) {
	var analyzed, action *time.Time
	err := r.DB.QueryRow(ctx, eventsSQL, n.Qualified, n.Unqualified, n.Objects, capturedAt).
		Scan(&analyzed, &action)
	if err != nil {
		return Events{}, err
	}
	var ev Events
	if analyzed != nil {
		ev.Analyzed = *analyzed
	}
	if action != nil {
		ev.Action = *action
	}
	return ev, nil
}
