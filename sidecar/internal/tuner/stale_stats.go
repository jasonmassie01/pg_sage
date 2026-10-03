package tuner

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StaleStatsCache holds per-table staleness information computed
// once per tuner cycle from pg_class + pg_stat_user_tables.
// All reads go through IsStale which combines mod-ratio and
// age gates from TunerConfig.
type StaleStatsCache struct {
	cfg     TunerConfig
	entries map[string]staleStatsEntry
}

type staleStatsEntry struct {
	liveTuples      int64
	modSinceAnalyze int64
	lastAnalyze     time.Time // zero if never analyzed
	lastAutoAnalyze time.Time
	sizeMB          int64
}

// staleStatsSQL reads the analyze counters by oid with the pg_stat_get_*
// functions pg_stat_user_tables is built from, and sizes from relpages
// (main fork, as of the last VACUUM/ANALYZE): no relation is opened and
// no grouped view is built (measured.md M13: a name join to the view plus
// pg_relation_size per table, 98-350 ms per tuner cycle on lifeos).
const staleStatsSQL = `/* pg_sage tuner:stale_stats */
SELECT
    n.nspname AS schemaname,
    c.relname AS tablename,
    pg_stat_get_live_tuples(c.oid) AS n_live_tup,
    pg_stat_get_mod_since_analyze(c.oid) AS n_mod,
    pg_stat_get_last_analyze_time(c.oid) AS last_analyze,
    pg_stat_get_last_autoanalyze_time(c.oid) AS last_autoanalyze,
    (c.relpages::int8 * current_setting('block_size')::int8 / (1024*1024))::bigint
        AS size_mb
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','m','p')
  AND n.nspname NOT IN ('pg_catalog','information_schema','sage','hint_plan')
  AND n.nspname NOT LIKE 'pg_toast%'
  AND n.nspname NOT LIKE 'pg_temp%'
`

// LoadStaleStatsCache queries pg_class joined with
// pg_stat_user_tables for every regular, materialized, or
// partitioned table and returns a populated cache ready for
// IsStale() lookups.
func LoadStaleStatsCache(
	ctx context.Context, pool *pgxpool.Pool, cfg TunerConfig,
) (*StaleStatsCache, error) {
	cache := &StaleStatsCache{
		cfg:     cfg,
		entries: map[string]staleStatsEntry{},
	}
	if pool == nil {
		return cache, nil
	}
	rows, err := pool.Query(ctx, staleStatsSQL)
	if err != nil {
		return cache, fmt.Errorf("load stale stats cache: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var schema, table string
		var liveTup, modSince, sizeMB int64
		var lastA, lastAA *time.Time
		if err := rows.Scan(
			&schema, &table, &liveTup, &modSince,
			&lastA, &lastAA, &sizeMB,
		); err != nil {
			return cache, fmt.Errorf("scan stale stats: %w", err)
		}
		e := staleStatsEntry{
			liveTuples:      liveTup,
			modSinceAnalyze: modSince,
			sizeMB:          sizeMB,
		}
		if lastA != nil {
			e.lastAnalyze = *lastA
		}
		if lastAA != nil {
			e.lastAutoAnalyze = *lastAA
		}
		cache.entries[CanonicalTableName(schema, table)] = e
	}
	return cache, rows.Err()
}

// IsStale returns true when the canonical "schema.table" entry
// crosses both the mod-ratio gate (n_mod_since_analyze /
// n_live_tup >= cfg.StaleStatsModRatio) and the age gate
// (most recent analyze older than cfg.StaleStatsAgeMinutes).
// Tables not in the cache return false.
func (c *StaleStatsCache) IsStale(canonicalTable string) bool {
	if c == nil || c.entries == nil {
		return false
	}
	e, ok := c.entries[canonicalTable]
	if !ok {
		return false
	}
	// Mod ratio gate. Require at least 1 modification.
	if e.modSinceAnalyze <= 0 {
		return false
	}
	if e.liveTuples > 0 {
		ratio := float64(e.modSinceAnalyze) / float64(e.liveTuples)
		if ratio < c.cfg.StaleStatsModRatio {
			return false
		}
	}
	// Age gate (only if the table has ever been analyzed).
	newest := latestTime(e.lastAnalyze, e.lastAutoAnalyze)
	if !newest.IsZero() {
		minAge := time.Duration(c.cfg.StaleStatsAgeMinutes) * time.Minute
		if time.Since(newest) < minAge {
			return false
		}
	}
	return true
}

// SizeMB returns the on-disk size of the canonical table in MB,
// or 0 if the table is not in the cache.
func (c *StaleStatsCache) SizeMB(canonicalTable string) int64 {
	if c == nil || c.entries == nil {
		return 0
	}
	return c.entries[canonicalTable].sizeMB
}

func latestTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// annotateForStaleStats inspects the symptom list and, when at
// least one SymptomBadNestedLoop is present, checks whether any
// relation touched by the plan has stale statistics. If so, the
// bad_nested_loop symptoms are dropped (their HashJoin hint
// would mask the real problem) and replaced with one
// SymptomStaleStats per stale table so the tuner emits an
// ANALYZE finding instead.
func annotateForStaleStats(
	planJSON []byte,
	symptoms []PlanSymptom,
	cache *StaleStatsCache,
) []PlanSymptom {
	if cache == nil || len(symptoms) == 0 || len(planJSON) == 0 {
		return symptoms
	}
	hasBNL := false
	for _, s := range symptoms {
		if s.Kind == SymptomBadNestedLoop {
			hasBNL = true
			break
		}
	}
	if !hasBNL {
		return symptoms
	}
	rels, err := ExtractRelations(planJSON)
	if err != nil || len(rels) == 0 {
		return symptoms
	}
	var staleTables []string
	for rel := range rels {
		if cache.IsStale(rel) {
			staleTables = append(staleTables, rel)
		}
	}
	if len(staleTables) == 0 {
		return symptoms
	}
	// Drop bad_nested_loop; keep everything else.
	out := make([]PlanSymptom, 0, len(symptoms)+len(staleTables))
	for _, s := range symptoms {
		if s.Kind == SymptomBadNestedLoop {
			continue
		}
		out = append(out, s)
	}
	for _, t := range staleTables {
		schema, table := SplitCanonical(t)
		out = append(out, PlanSymptom{
			Kind:         SymptomStaleStats,
			RelationName: table,
			Schema:       schema,
			Detail: map[string]any{
				"canonical": t,
				"trigger":   "nested_loop_misestimate",
			},
		})
	}
	return out
}
