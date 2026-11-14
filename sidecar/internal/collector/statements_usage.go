package collector

import (
	"context"
	"errors"
	"time"

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

// classifyEvery spaces the classifications, which read every entry's
// text; the entry count is read every cycle. Past it, the texts are
// classified again only when the entry count or the deallocations moved
// (collection-11).
const classifyEvery = time.Hour

// classCache is the last classification's counts, when it was read and
// the entry and deallocation counts it saw.
type classCache struct {
	at               time.Time
	utility, copyOut int
	entries          int
	dealloc          int64
}

// classificationCurrent reports whether cached still describes the
// entries u reports: inside classifyEvery, or past it with the same entry
// count and the same known deallocation count.
func classificationCurrent(cached classCache, now time.Time, u *StatStatementsUsage) bool {
	switch {
	case cached.at.IsZero():
		return false
	case now.Sub(cached.at) < classifyEvery:
		return true
	}
	return u.Dealloc >= 0 && u.Dealloc == cached.dealloc && u.Entries == cached.entries
}

// nearStatementsCapacity reports entries at or above classifyPercent of max.
func nearStatementsCapacity(entries, max int) bool {
	return max > 0 && entries > 0 && entries*100 >= max*classifyPercent
}

// statementsEntriesSQL counts entries without reading their texts; fn is
// the qualified pg_stat_statements function.
func statementsEntriesSQL(fn string) string {
	return sageTag + `SELECT count(*)::int FROM ` + fn + `(false)`
}

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

// statementsSettingsSQL reads track_utility and whether the info view
// (pg_stat_statements 1.9+, PostgreSQL 14+) exists; $1 is its qualified
// name.
const statementsSettingsSQL = sageTag + `
SELECT COALESCE(current_setting('pg_stat_statements.track_utility', true), ''),
       to_regclass($1) IS NOT NULL`

// statementsDeallocSQL reads the deallocations from info, the qualified
// info view.
func statementsDeallocSQL(info string) string {
	return sageTag + `SELECT dealloc::int8 FROM ` + info
}

// collectStatStatementsUsage reads the usage when pg_stat_statements is
// loaded (max > 0), classifying texts only near capacity. A failed read
// is logged and leaves the usage unknown (nil), never zero.
func (c *Collector) collectStatStatementsUsage(ctx context.Context,
	max int) *StatStatementsUsage {
	if max <= 0 {
		return nil
	}
	var entries int
	names, err := c.pgss()
	if err == nil {
		err = c.catalogQueryRow(ctx, statementsEntriesSQL(names.fn)).Scan(&entries)
	}
	var u *StatStatementsUsage
	switch {
	case err != nil:
	case nearStatementsCapacity(entries, max):
		u, err = c.classifiedUsage(ctx, entries)
	default:
		u = &StatStatementsUsage{Entries: entries, Dealloc: -1}
		err = c.readStatementsSettings(ctx, u)
	}
	if err != nil {
		c.logFn("WARN", "collector: pg_stat_statements usage unavailable this cycle: %v",
			err)
		return nil
	}
	c.stmtTexts.observeDealloc(u.Dealloc)
	return u
}

// classifiedUsage classifies the texts while classificationCurrent is
// false and reuses the last counts otherwise, with the fresh entry count.
func (c *Collector) classifiedUsage(ctx context.Context,
	entries int) (*StatStatementsUsage, error) {
	now := c.clock()
	u := &StatStatementsUsage{Entries: entries, Classified: true, Dealloc: -1}
	if err := c.readStatementsSettings(ctx, u); err != nil {
		return nil, err
	}
	if cached := c.pgssClass; classificationCurrent(cached, now, u) {
		u.Utility, u.CopyOut = min(cached.utility, entries), min(cached.copyOut, entries)
		return u, nil
	}
	u, err := c.readStatStatementsUsage(ctx, true)
	if err != nil {
		return nil, err
	}
	c.pgssClass = classCache{at: now, utility: u.Utility, copyOut: u.CopyOut,
		entries: u.Entries, dealloc: u.Dealloc}
	return u, nil
}

// readStatStatementsUsage reads the entry count, the settings and the
// deallocations; with classify, the utility and COPY ... TO STDOUT counts.
func (c *Collector) readStatStatementsUsage(ctx context.Context,
	classify bool) (*StatStatementsUsage, error) {
	u := &StatStatementsUsage{Dealloc: -1, Classified: classify}
	names, err := c.pgss()
	switch {
	case err != nil:
	case classify:
		err = c.catalogQueryRow(ctx, classifySQL(names.view)).Scan(&u.Entries,
			&u.Utility, &u.CopyOut)
	default:
		err = c.catalogQueryRow(ctx, statementsEntriesSQL(names.fn)).Scan(&u.Entries)
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
	names, err := c.pgss()
	if err != nil {
		return err
	}
	var hasInfo bool
	if err := c.catalogQueryRow(ctx, statementsSettingsSQL, names.info).Scan(
		&u.TrackUtility, &hasInfo); err != nil {
		return err
	}
	if !hasInfo {
		return nil
	}
	err = c.catalogQueryRow(ctx, statementsDeallocSQL(names.info)).Scan(&u.Dealloc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
