package selfcost

import (
	"context"
	"fmt"
	"time"
)

// readCounters reads this database's entries without their texts, and
// pg_stat_statements_info.stats_reset (zero on 1.8 or with no entry). A
// key reported twice (a 1.8 read of a server since updated) is summed.
func readCounters(ctx context.Context, q Querier, topLevel bool) (
	map[StatementKey]StatementCounters, time.Time, error) {
	sql := countersSQL18
	if topLevel {
		sql = countersSQL
	}
	var reset time.Time
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, reset, err
	}
	defer rows.Close()
	out := map[StatementKey]StatementCounters{}
	for rows.Next() {
		var k StatementKey
		var c StatementCounters
		var at *time.Time
		if err := rows.Scan(&k.UserID, &k.QueryID, &k.TopLevel, &c.TimeMs, &c.Calls,
			&c.Blocks, &at); err != nil {
			return nil, reset, fmt.Errorf("scan counters: %w", err)
		}
		if at != nil {
			reset = *at
		}
		out[k] = sum(out[k], c)
	}
	return out, reset, rows.Err()
}

// readTexts reads the tagged entries with their texts.
func readTexts(ctx context.Context, q Querier, topLevel bool) (
	map[StatementKey]StatementCounters, error) {
	sql := textsSQL18
	if topLevel {
		sql = textsSQL
	}
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[StatementKey]StatementCounters{}
	for rows.Next() {
		var k StatementKey
		var c StatementCounters
		if err := rows.Scan(&k.UserID, &k.QueryID, &k.TopLevel, &c.TimeMs, &c.Calls,
			&c.Blocks, &c.Text); err != nil {
			return nil, fmt.Errorf("scan tagged text: %w", err)
		}
		out[k] = sum(out[k], c)
	}
	return out, rows.Err()
}

// sum adds c's counters to a; a's text is kept when it has one.
func sum(a, c StatementCounters) StatementCounters {
	if a.Text == "" {
		a.Text = c.Text
	}
	a.TimeMs += c.TimeMs
	a.Calls += c.Calls
	a.Blocks += c.Blocks
	return a
}

// revalidate drops what the counters prove stale: every key when
// stats_reset moved, a key gone from pg_stat_statements (evicted), and a
// key whose calls went back (reset or evicted and re-created).
func (s *Sampler) revalidate(counters map[StatementKey]StatementCounters, reset time.Time) {
	if !reset.IsZero() {
		if !s.reset.IsZero() && !reset.Equal(s.reset) {
			s.keys = map[StatementKey]classified{}
		}
		s.reset = reset
	}
	for k, cached := range s.keys {
		if c, ok := counters[k]; !ok || c.Calls < cached.calls {
			delete(s.keys, k)
		}
	}
}

// unclassified reports keys in the counters the cache does not hold.
func (s *Sampler) unclassified(counters map[StatementKey]StatementCounters) bool {
	for k := range counters {
		if _, ok := s.keys[k]; !ok {
			return true
		}
	}
	return false
}

// classify returns pg_sage's entries of the counters, with their texts,
// and caches each key. texts (nil when every key was cached) are the
// tagged entries; one only there appeared after the counters were read.
func (s *Sampler) classify(
	counters, texts map[StatementKey]StatementCounters,
) map[StatementKey]StatementCounters {
	out := map[StatementKey]StatementCounters{}
	for k, c := range counters {
		entry, ok := s.keys[k]
		if !ok {
			t, sage := texts[k]
			entry = classified{sage: sage, text: t.Text}
		}
		entry.calls = c.Calls
		s.remember(k, entry)
		if entry.sage {
			c.Text = entry.text
			out[k] = c
		}
	}
	for k, t := range texts {
		if _, ok := counters[k]; !ok {
			out[k] = t
			s.remember(k, classified{sage: true, calls: t.Calls, text: t.Text})
		}
	}
	return out
}

// remember caches a key within MaxClassifiedKeys. A full cache keeps
// pg_sage's keys (their texts are what findings show) by dropping an
// application key; an application key it has no room for is not kept.
func (s *Sampler) remember(k StatementKey, e classified) {
	if _, ok := s.keys[k]; ok || len(s.keys) < MaxClassifiedKeys {
		s.keys[k] = e
		return
	}
	if !e.sage {
		return
	}
	for old, v := range s.keys {
		if !v.sage {
			delete(s.keys, old)
			s.keys[k] = e
			return
		}
	}
}
