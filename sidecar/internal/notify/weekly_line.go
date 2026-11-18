package notify

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// The weekly line (ux-09b): "N messages, M led to an action". Over the
// last 7 days N counts the sent deliveries in sage.notification_log. M
// counts those followed within 24 h by a decision on the same item: an
// approve, reject or snooze of a queue item the message carried
// (item_refs "queue:<id>"), or a mute of the message's database. The line
// is shown on the Notifications page and in the digest.

const (
	weeklyWindow = 7 * 24 * time.Hour
	actionWindow = 24 * time.Hour
	// weeklyRowLimit bounds each read of the line.
	weeklyRowLimit = 100000
)

// DataItemRefs lists the items ("queue:<id>") a message carries when it
// carries more than one (a batched Ask).
const DataItemRefs = "item_refs"

// LineDelivery is one sent message.
type LineDelivery struct {
	SentAt   time.Time
	Database string
	Items    []string
}

// LineDecision is one decision a message may have led to. A mute names a
// database; any other verb names an item.
type LineDecision struct {
	At       time.Time
	Item     string
	Database string
	Verb     string // approve, reject, snooze, mute
}

// WeeklyLine is the line and its numbers.
type WeeklyLine struct {
	Messages    int       `json:"messages"`
	LedToAction int       `json:"led_to_action"`
	Text        string    `json:"text"`
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until"`
}

// ComputeWeeklyLine is the line at now over deliveries sent in [now-7d,
// now]. A message led to an action when a decision on one of its items
// (or a mute of its database) came at or after it and at most 24 h later.
// The decisions are indexed once, so the line stays linear in its rows.
func ComputeWeeklyLine(ds []LineDelivery, decs []LineDecision, now time.Time) WeeklyLine {
	line := WeeklyLine{Since: now.Add(-weeklyWindow), Until: now}
	idx := indexDecisions(decs)
	for _, d := range ds {
		if d.SentAt.Before(line.Since) || d.SentAt.After(now) {
			continue
		}
		line.Messages++
		if idx.ledToAction(d) {
			line.LedToAction++
		}
	}
	line.Text = weeklyText(line.Messages, line.LedToAction)
	return line
}

// decisionIndex holds each item's decision times and each database's
// mute times, sorted.
type decisionIndex struct{ items, mutes map[string][]time.Time }

func indexDecisions(decs []LineDecision) decisionIndex {
	idx := decisionIndex{items: map[string][]time.Time{}, mutes: map[string][]time.Time{}}
	for _, dec := range decs {
		switch {
		case dec.Verb == "mute" && dec.Database != "":
			idx.mutes[dec.Database] = append(idx.mutes[dec.Database], dec.At)
		case dec.Verb != "mute" && dec.Item != "":
			idx.items[dec.Item] = append(idx.items[dec.Item], dec.At)
		}
	}
	for _, m := range []map[string][]time.Time{idx.items, idx.mutes} {
		for _, ts := range m {
			slices.SortFunc(ts, time.Time.Compare)
		}
	}
	return idx
}

func (idx decisionIndex) ledToAction(d LineDelivery) bool {
	if d.Database != "" && within(idx.mutes[d.Database], d.SentAt) {
		return true
	}
	for _, item := range d.Items {
		if within(idx.items[item], d.SentAt) {
			return true
		}
	}
	return false
}

// within reports whether a sorted time in ts falls in [at, at+24h].
func within(ts []time.Time, at time.Time) bool {
	i, _ := slices.BinarySearchFunc(ts, at, time.Time.Compare)
	return i < len(ts) && !ts[i].After(at.Add(actionWindow))
}

func weeklyText(n, m int) string {
	switch n {
	case 0:
		return "No messages this week"
	case 1:
		return fmt.Sprintf("1 message, %d led to an action", m)
	}
	return fmt.Sprintf("%d messages, %d led to an action", n, m)
}

// eventItemRefs is the items evt carries: its queue item, then the items
// of a batch, each once.
func eventItemRefs(evt Event) []string {
	var out []string
	add := func(ref string) {
		if ref != "" && !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	if id := positiveInt(evt.Data["queue_id"]); id > 0 {
		add("queue:" + strconv.FormatInt(id, 10))
	}
	switch refs := evt.Data[DataItemRefs].(type) {
	case []string:
		for _, r := range refs {
			add(r)
		}
	case []any:
		for _, r := range refs {
			s, _ := r.(string)
			add(s)
		}
	}
	return out
}

// positiveInt reads a queue id as event data holds it: an int, or a
// float64 after a JSON round trip; anything else is 0.
func positiveInt(v any) int64 {
	switch n := v.(type) {
	case int:
		return max(int64(n), 0)
	case int64:
		return max(n, 0)
	case float64:
		if n > 0 && n == float64(int64(n)) {
			return int64(n)
		}
	}
	return 0
}

// WeeklyLine reads the last 7 days before now and computes the line.
func (s *PoolStore) WeeklyLine(ctx context.Context, now time.Time) (WeeklyLine, error) {
	if s.pool == nil {
		return WeeklyLine{}, fmt.Errorf("weekly line: notification pool is nil")
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	since := now.Add(-weeklyWindow)
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT sent_at, COALESCE(database, ''),
		       COALESCE(item_refs, '{}')
		  FROM sage.notification_log
		 WHERE status = 'sent' AND event <> 'test' AND sent_at >= $1 AND sent_at <= $2
		 ORDER BY sent_at LIMIT $3`, since, now, weeklyRowLimit)
	if err != nil {
		return WeeklyLine{}, fmt.Errorf("weekly line: read deliveries: %w", err)
	}
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LineDelivery, error) {
		var d LineDelivery
		return d, r.Scan(&d.SentAt, &d.Database, &d.Items)
	})
	if err != nil {
		return WeeklyLine{}, fmt.Errorf("weekly line: read deliveries: %w", err)
	}
	decs, err := s.lineDecisions(qctx, since, now.Add(actionWindow))
	if err != nil {
		return WeeklyLine{}, err
	}
	return ComputeWeeklyLine(ds, decs, now), nil
}

// lineDecisionsSQL is every decision in [$1, $2]: approvals and
// rejections (decided_at), snoozes (snoozed_at) and mutes (created_at).
const lineDecisionsSQL = `/* pg_sage */ SELECT at, item, database, verb FROM (
    SELECT decided_at AS at, 'queue:' || id AS item, '' AS database, 'decide' AS verb
      FROM sage.action_queue WHERE decided_at >= $1 AND decided_at <= $2
    UNION ALL
    SELECT snoozed_at, 'queue:' || id, '', 'snooze'
      FROM sage.action_queue WHERE snoozed_at >= $1 AND snoozed_at <= $2
    UNION ALL
    SELECT created_at, '', database, 'mute'
      FROM sage.notification_mutes WHERE created_at >= $1 AND created_at <= $2
) d LIMIT $3`

func (s *PoolStore) lineDecisions(ctx context.Context, from, to time.Time) (
	[]LineDecision, error) {
	rows, err := s.pool.Query(ctx, lineDecisionsSQL, from, to, weeklyRowLimit)
	if err != nil {
		return nil, fmt.Errorf("weekly line: read decisions: %w", err)
	}
	decs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LineDecision, error) {
		var d LineDecision
		return d, r.Scan(&d.At, &d.Item, &d.Database, &d.Verb)
	})
	if err != nil {
		return nil, fmt.Errorf("weekly line: read decisions: %w", err)
	}
	return decs, nil
}
