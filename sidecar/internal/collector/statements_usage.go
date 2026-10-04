package collector

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// StatStatementsUsage is how full pg_stat_statements is, and with what
// (dogfood lifeos, 2026-10-04: 4,859 of 5,000 entries, 3,701 of them
// pg_dump "COPY ... TO stdout" statements, 100 deallocations).
type StatStatementsUsage struct {
	// Entries is the number of entries, every database and user.
	Entries int `json:"entries"`
	// Classified reports whether Utility and CopyOut were counted: only
	// near capacity, since it reads every query text.
	Classified bool `json:"classified"`
	// Utility counts entries that are not SELECT, INSERT, UPDATE,
	// DELETE, MERGE, WITH, VALUES or TABLE statements.
	Utility int `json:"utility"`
	// CopyOut counts "COPY ... TO STDOUT" entries (pg_dump, exports).
	CopyOut int `json:"copy_out"`
	// Dealloc is pg_stat_statements_info.dealloc; -1 when unknown.
	Dealloc int64 `json:"dealloc"`
	// TrackUtility is pg_stat_statements.track_utility ("on", "off").
	TrackUtility string `json:"track_utility"`
}

// classifyPercent is how full pg_stat_statements must be before its
// texts are classified: the analyzer's capacity warning starts there.
const classifyPercent = 80

// nearStatementsCapacity reports entries at or above classifyPercent of max.
func nearStatementsCapacity(entries, max int) bool {
	return max > 0 && entries > 0 && entries*100 >= max*classifyPercent
}

// statementsEntriesSQL counts entries without reading their texts.
const statementsEntriesSQL = sageTag + `SELECT count(*)::int FROM pg_stat_statements(false)`

// classifySQL counts the entries of source, the utility ones and the
// COPY ... TO STDOUT ones, by each text's first keyword after leading
// comments and parentheses. A text the role may not see (<insufficient
// privilege>) is neither utility nor application.
func classifySQL(source string) string {
	return sageTag + `
SELECT count(*)::int,
       count(*) FILTER (WHERE kw NOT IN ('select', 'insert', 'update', 'delete', 'merge',
                                         'with', 'values', 'table'))::int,
       count(*) FILTER (WHERE kw = 'copy' AND query ~* '\mto\s+stdout\M')::int
FROM (SELECT query,
             lower(substring(query FROM '^(?:\s|/\*[^*]*\*/|\()*([A-Za-z]+)')) AS kw
      FROM ` + source + `) s`
}

// statementsClassifySQL classifies every pg_stat_statements entry.
var statementsClassifySQL = classifySQL("pg_stat_statements")

// statementsSettingsSQL reads track_utility and whether the info view
// (pg_stat_statements 1.9+, PostgreSQL 14+) exists.
const statementsSettingsSQL = sageTag + `
SELECT COALESCE(current_setting('pg_stat_statements.track_utility', true), ''),
       to_regclass('pg_stat_statements_info') IS NOT NULL`

const statementsDeallocSQL = sageTag + `SELECT dealloc::int8 FROM pg_stat_statements_info`

// collectStatStatementsUsage reads the usage when pg_stat_statements is
// loaded (max > 0), classifying texts only near capacity. A failed read
// is logged and leaves the usage unknown (nil), never zero.
func (c *Collector) collectStatStatementsUsage(ctx context.Context,
	max int) *StatStatementsUsage {
	if max <= 0 {
		return nil
	}
	var entries int
	err := c.catalogQueryRow(ctx, statementsEntriesSQL).Scan(&entries)
	var u *StatStatementsUsage
	switch {
	case err != nil:
	case nearStatementsCapacity(entries, max):
		u, err = c.readStatStatementsUsage(ctx, true)
	default:
		u = &StatStatementsUsage{Entries: entries, Dealloc: -1}
		err = c.readStatementsSettings(ctx, u)
	}
	if err != nil {
		c.logFn("WARN", "collector: pg_stat_statements usage unavailable this cycle: %v",
			err)
		return nil
	}
	return u
}

// readStatStatementsUsage reads the entry count, the settings and the
// deallocations; with classify, the utility and COPY ... TO STDOUT counts.
func (c *Collector) readStatStatementsUsage(ctx context.Context,
	classify bool) (*StatStatementsUsage, error) {
	u := &StatStatementsUsage{Dealloc: -1, Classified: classify}
	var err error
	if classify {
		err = c.catalogQueryRow(ctx, statementsClassifySQL).Scan(&u.Entries, &u.Utility,
			&u.CopyOut)
	} else {
		err = c.catalogQueryRow(ctx, statementsEntriesSQL).Scan(&u.Entries)
	}
	if err != nil {
		return nil, err
	}
	if err := c.readStatementsSettings(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// readStatementsSettings fills track_utility and, where the info view
// exists, the deallocation count.
func (c *Collector) readStatementsSettings(ctx context.Context, u *StatStatementsUsage) error {
	var hasInfo bool
	if err := c.catalogQueryRow(ctx, statementsSettingsSQL).Scan(&u.TrackUtility,
		&hasInfo); err != nil {
		return err
	}
	if !hasInfo {
		return nil
	}
	err := c.catalogQueryRow(ctx, statementsDeallocSQL).Scan(&u.Dealloc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
