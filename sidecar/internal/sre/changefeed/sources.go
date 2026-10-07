package changefeed

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// cursorBatch bounds the rows a cursor source reads per poll; the rest
// follow on the next poll.
const cursorBatch = 100

func (p *Poller) sources() []source {
	return []source{
		{name: "sage_actions", poll: pollActions},
		{name: "config_audit", control: true, poll: p.pollConfigAudit},
		{name: "migration_findings", poll: pollMigrationFindings},
		{name: "pg_stat_statements", poll: pollStatsReset},
		{name: "postmaster", poll: pollPostmaster},
		{name: "recovery_role", poll: pollRecoveryRole},
		{name: "timeline", poll: pollTimeline},
		{name: "extensions", poll: pollExtensions},
	}
}

type cursorState struct {
	LastID int64 `json:"last_id"`
}

func lastID(prev []byte) (int64, error) {
	var c cursorState
	if prev == nil {
		return 0, nil
	}
	if err := json.Unmarshal(prev, &c); err != nil {
		return 0, fmt.Errorf("cursor state: %w", err)
	}
	return c.LastID, nil
}

// cursorRow is one new row of a cursor source.
type cursorRow struct {
	id      int64
	summary string
	at      time.Time
}

// pollCursor reads rows after the cursor (within the last 7 days) and
// turns each into an event.
func pollCursor(ctx context.Context, q querier, prev []byte, now time.Time, kind Kind,
	idPrefix, sql string, args ...any) ([]Event, any, error) {
	last, err := lastID(prev)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, sql, append([]any{last, cursorBatch}, args...)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var r cursorRow
		if err := rows.Scan(&r.id, &r.summary, &r.at); err != nil {
			return nil, nil, err
		}
		events = append(events, internalEvent(kind, fmt.Sprintf("%s:%d", idPrefix, r.id),
			r.summary, r.at, now))
		last = max(last, r.id)
	}
	return events, cursorState{LastID: last}, rows.Err()
}

// pollActions reads pg_sage's own actions: type and outcome, never SQL.
func pollActions(ctx context.Context, q querier, prev []byte, now time.Time) ([]Event, any,
	error) {
	return pollCursor(ctx, q, prev, now, KindSageAction, "action",
		`/* pg_sage sre:change_feed */ SELECT id::int8,
		    'pg_sage ' || left(action_type, 64) || ' (' || left(outcome, 32) || ')',
		    executed_at
		FROM sage.action_log
		WHERE id > $1 AND executed_at > now() - interval '7 days'
		ORDER BY id LIMIT $2`)
}

// pollConfigAudit reads configuration changes for this database (or
// fleet-wide ones): the key and the actor, never the values.
func (p *Poller) pollConfigAudit(ctx context.Context, q querier, prev []byte,
	now time.Time) ([]Event, any, error) {
	return pollCursor(ctx, q, prev, now, KindConfig, "config_audit",
		`/* pg_sage sre:change_feed */ SELECT id::int8,
		    'pg_sage setting ' || left(key, 128) || ' changed'
		        || COALESCE(' by ' || left(changed_by_actor, 64), ''),
		    COALESCE(changed_at, now())
		FROM sage.config_audit
		WHERE id > $1 AND changed_at > now() - interval '7 days'
		  AND (database_id IS NULL OR database_id = $3::int)
		ORDER BY id LIMIT $2`, p.legacyID)
}

// pollMigrationFindings reads DDL the migration detector saw.
func pollMigrationFindings(ctx context.Context, q querier, prev []byte,
	now time.Time) ([]Event, any, error) {
	return pollCursor(ctx, q, prev, now, KindDDL, "finding", migrationFeedSQL)
}

// migrationFeedSQL reads the migration detector's findings past the
// cursor through idx_findings_migration_feed.
const migrationFeedSQL = `/* pg_sage sre:change_feed */ SELECT id::int8,
		    'migration detector: ' || left(title, 200), created_at
		FROM sage.findings
		WHERE category = 'migration_safety' AND id > $1
		  AND created_at > now() - interval '7 days'
		ORDER BY id LIMIT $2`

// snapshot polls one value; changed builds the event when it differs
// from the previous one. The first sight only records the baseline.
func snapshot[T comparable](ctx context.Context, q querier, prev []byte, field string,
	read func(context.Context, querier) (T, error),
	changed func(old, cur T) Event) ([]Event, any, error) {
	cur, err := read(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	state := map[string]T{field: cur}
	if prev == nil {
		return nil, state, nil
	}
	var old map[string]T
	if err := json.Unmarshal(prev, &old); err != nil {
		return nil, nil, fmt.Errorf("%s state: %w", field, err)
	}
	if old[field] == cur {
		return nil, state, nil
	}
	return []Event{changed(old[field], cur)}, state, nil
}

// scanOne reads a single-value query.
func scanOne[T any](sql string) func(context.Context, querier) (T, error) {
	return func(ctx context.Context, q querier) (T, error) {
		var v T
		err := q.QueryRow(ctx, sql).Scan(&v)
		return v, err
	}
}

// scanTime reads a timestamp as a canonical UTC string, so equal instants
// compare equal whatever their time zone.
func scanTime(sql string) func(context.Context, querier) (string, error) {
	return func(ctx context.Context, q querier) (string, error) {
		var t time.Time
		if err := q.QueryRow(ctx, sql).Scan(&t); err != nil {
			return "", err
		}
		return t.UTC().Format(time.RFC3339Nano), nil
	}
}

func parseUTC(s string, fallback time.Time) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fallback
	}
	return t
}

func pollStatsReset(ctx context.Context, q querier, prev []byte, now time.Time) ([]Event,
	any, error) {
	return snapshot(ctx, q, prev, "stats_reset", scanTime(
		`/* pg_sage sre:change_feed */ SELECT stats_reset FROM pg_stat_statements_info`),
		func(_, cur string) Event {
			return internalEvent(KindStatsReset, "pgss_reset:"+cur,
				"pg_stat_statements statistics were reset", parseUTC(cur, now), now)
		})
}

func pollPostmaster(ctx context.Context, q querier, prev []byte, now time.Time) ([]Event,
	any, error) {
	return snapshot(ctx, q, prev, "start", scanTime(
		`/* pg_sage sre:change_feed */ SELECT pg_postmaster_start_time()`),
		func(_, cur string) Event {
			return internalEvent(KindRestart, "restart:"+cur,
				"PostgreSQL restarted (postmaster start time changed)", parseUTC(cur, now), now)
		})
}

func pollRecoveryRole(ctx context.Context, q querier, prev []byte, now time.Time) ([]Event,
	any, error) {
	return snapshot(ctx, q, prev, "in_recovery", scanOne[bool](
		`/* pg_sage sre:change_feed */ SELECT pg_is_in_recovery()`),
		func(_, cur bool) Event {
			role, text := "primary", "server promoted from standby to primary (failover)"
			if cur {
				role, text = "standby", "server became a standby (failover or rebuild)"
			}
			return internalEvent(KindFailover, fmt.Sprintf("role:%s:%d", role, now.Unix()),
				text, now, now)
		})
}

func pollTimeline(ctx context.Context, q querier, prev []byte, now time.Time) ([]Event,
	any, error) {
	return snapshot(ctx, q, prev, "timeline", scanOne[int64](
		`/* pg_sage sre:change_feed */ SELECT timeline_id::int8 FROM pg_control_checkpoint()`),
		func(old, cur int64) Event {
			return internalEvent(KindFailover, fmt.Sprintf("timeline:%d->%d", old, cur),
				fmt.Sprintf("timeline changed %d -> %d (failover or point-in-time recovery)",
					old, cur), now, now)
		})
}

type extensionState struct {
	Versions map[string]string `json:"versions"`
}

func pollExtensions(ctx context.Context, q querier, prev []byte, now time.Time) ([]Event,
	any, error) {
	rows, err := q.Query(ctx, `/* pg_sage sre:change_feed */
		SELECT extname::text, extversion FROM pg_extension ORDER BY extname`)
	if err != nil {
		return nil, nil, err
	}
	cur := extensionState{Versions: map[string]string{}}
	for rows.Next() {
		var name, version string
		if err := rows.Scan(&name, &version); err != nil {
			rows.Close()
			return nil, nil, err
		}
		cur.Versions[name] = version
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if prev == nil {
		return nil, cur, nil
	}
	var old extensionState
	if err := json.Unmarshal(prev, &old); err != nil {
		return nil, nil, fmt.Errorf("extensions state: %w", err)
	}
	return extensionEvents(old.Versions, cur.Versions, now), cur, nil
}

// extensionEvents lists installs, upgrades and removals, by name.
func extensionEvents(old, cur map[string]string, now time.Time) []Event {
	names := map[string]bool{}
	for n := range old {
		names[n] = true
	}
	for n := range cur {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	var out []Event
	for _, n := range sorted {
		was, had := old[n]
		is, has := cur[n]
		var text string
		switch {
		case had && has && was != is:
			text = fmt.Sprintf("extension %s %s -> %s", n, was, is)
		case had && !has:
			text, is = fmt.Sprintf("extension %s %s removed", n, was), "removed"
		case !had && has:
			text, was = fmt.Sprintf("extension %s %s installed", n, is), "none"
		default:
			continue
		}
		out = append(out, internalEvent(KindExtension,
			fmt.Sprintf("extension:%s:%s->%s", n, was, is), text, now, now))
	}
	return out
}
