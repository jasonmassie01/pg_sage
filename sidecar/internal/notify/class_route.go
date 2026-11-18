package notify

import (
	"context"
	"fmt"
	"time"
)

// Class rules (ux-09a). A rule whose delivery_class is set routes every
// event of that class to its channel; a legacy rule routes one event type.
// A channel that a legacy rule already serves for an event gets no second
// copy from a class rule. Digest-class events reach a class rule only as
// the digest itself (ux-02b builds it), never one by one.

// ClassRuleStore reads class rules; a store without it has none.
type ClassRuleStore interface {
	ClassRules(ctx context.Context, c Class) ([]Rule, error)
}

// ClassRules returns the enabled class rules of class c.
func (s *PoolStore) ClassRules(ctx context.Context, c Class) ([]Rule, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("query class rules: notification pool is nil")
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx,
		`/* pg_sage */ SELECT id, channel_id, event, COALESCE(min_severity, 'info'),
		        delivery_class
		 FROM sage.notification_rules
		 WHERE delivery_class = $1 AND enabled = true`, string(c))
	if err != nil {
		return nil, fmt.Errorf("query %s class rules: %w", c, err)
	}
	defer rows.Close()
	var rules []Rule
	for rows.Next() {
		r := Rule{Enabled: true}
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.Event, &r.MinSeverity,
			&r.DeliveryClass); err != nil {
			return nil, fmt.Errorf("scan class rule: %w", err)
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// SeedClassRules gives channelID the Page, Ask and Digest class rules when
// it is the install's only alert channel and no rule exists yet (a fresh
// install). It reports how many rules it wrote: 3, or 0 when the install
// already routes. The rows are API rules, so the Rules tab edits them.
func SeedClassRules(ctx context.Context, db execer, channelID int) (int, error) {
	tag, err := db.Exec(ctx, `/* pg_sage */ INSERT INTO sage.notification_rules
		(channel_id, event, min_severity, enabled, delivery_class)
		SELECT $1, 'class:' || c, 'info', true, c
		  FROM unnest($2::text[]) AS c
		 WHERE (SELECT count(*) FROM sage.notification_channels WHERE type <> 'jira') = 1
		   AND EXISTS (SELECT 1 FROM sage.notification_channels
		                WHERE id = $1 AND type <> 'jira')
		   AND NOT EXISTS (SELECT 1 FROM sage.notification_rules)`,
		channelID, []string{string(ClassPage), string(ClassAsk), string(ClassDigest)})
	if err != nil {
		return 0, fmt.Errorf("seed class rules for channel %d: %w", channelID, err)
	}
	return int(tag.RowsAffected()), nil
}

// matchingRules is every rule event fires: its event rules, then the class
// rules of its class on channels no event rule serves. A failed class
// lookup is logged and the event rules still fire.
func (d *Dispatcher) matchingRules(ctx context.Context, event Event) ([]Rule, error) {
	rules, err := d.loadMatchingRules(ctx, event.Type)
	if err != nil {
		return nil, err
	}
	cs, ok := d.store.(ClassRuleStore)
	class := ClassOf(event)
	if !ok || (class == ClassDigest && event.Type != EventDigestReady) {
		return rules, nil
	}
	classRules, err := cs.ClassRules(ctx, class)
	if err != nil {
		d.logFn("ERROR", "%s event: class rules not read, event rules only: %v",
			event.Type, err)
		return rules, nil
	}
	served := map[int]bool{}
	for _, r := range rules {
		served[r.ChannelID] = true
	}
	for _, r := range classRules {
		if !served[r.ChannelID] {
			served[r.ChannelID] = true
			rules = append(rules, r)
		}
	}
	return rules, nil
}
