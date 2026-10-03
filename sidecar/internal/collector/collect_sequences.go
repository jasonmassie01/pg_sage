package collector

import (
	"context"
	"fmt"
)

// Sequences kept per snapshot: every used sequence at or above
// SequenceFloorPct of its range, plus the SequenceTopN most used, at most
// SequenceMaxRows. The 75% exhaustion rule and the forecaster (which
// ignores sequences under 1%) see every sequence that can matter to them.
const (
	SequenceFloorPct = 1.0
	SequenceTopN     = 100
	SequenceMaxRows  = 1000
)

// SequenceCoverage says how much of the sequence catalog one cycle read
// (as the SRE sequence_runway probe reports its coverage).
type SequenceCoverage struct {
	Scanned    int  `json:"scanned"`    // catalog rows read this cycle
	Unreadable int  `json:"unreadable"` // of those, last value not readable by the role
	Used       int  `json:"used"`       // used sequences known (this and earlier cycles)
	Complete   bool `json:"complete"`   // the whole catalog was read this cycle
}

// collectSequences reads the sequence catalog in oid pages, one
// transaction per page, each holding at most SequencePageSize (and a
// quarter of the lock table) sequence locks (measured.md M4: one
// transaction held 12,012 locks on lifeos). At most SequenceScanCap are
// read per cycle; beyond that the read resumes where it stopped, and the
// rest is served from the last reads.
func (c *Collector) collectSequences(ctx context.Context) ([]SequenceStats, error) {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	pageSize, scanCap := positiveOr(c.seqPageSize, SequencePageSize),
		positiveOr(c.seqScanCap, SequenceScanCap)
	start := c.sequences.cursor
	cursor := start
	cov := SequenceCoverage{}
	for cov.Scanned < scanCap {
		page, limit, err := c.readSequencePage(ctx, cursor, min(pageSize, scanCap-cov.Scanned))
		if err != nil {
			c.seqCoverage = cov
			return nil, err
		}
		final := len(page) < limit
		c.sequences.apply(cursor, page, final)
		for _, r := range page {
			if !r.readable {
				cov.Unreadable++
			}
		}
		cov.Scanned += len(page)
		if final {
			cov.Complete = start == 0
			cursor = 0
			break
		}
		cursor = page[len(page)-1].oid
	}
	c.sequences.cursor = cursor
	all := c.sequences.list()
	cov.Used = len(all)
	c.seqCoverage = cov
	if !cov.Complete {
		c.logFn("INFO", "collector: read %d sequences this cycle (scan cap %d); "+
			"the rest are from earlier cycles", cov.Scanned, scanCap)
	}
	return selectSequences(all, SequenceFloorPct, SequenceTopN, SequenceMaxRows), nil
}

// sequenceCoverage is the coverage of the last sequence read.
func (c *Collector) sequenceCoverage() SequenceCoverage {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	return c.seqCoverage
}

// readSequencePage reads up to limit sequences after oid `after`; the
// returned page limit is limit after the lock-budget clamp.
func (c *Collector) readSequencePage(
	ctx context.Context, after uint32, limit int,
) ([]sequenceRow, int, error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("sequence page size %d: must be positive", limit)
	}
	rows, err := c.catalogQuery(ctx, sequencePageSQL, after, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("sequence page after oid %d: %w", after, err)
	}
	defer rows.Close()
	var page []sequenceRow
	pageLimit := limit
	for rows.Next() {
		r, lim, err := scanSequenceRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("sequence page after oid %d: %w", after, err)
		}
		page, pageLimit = append(page, r), lim
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("sequence page after oid %d: %w", after, err)
	}
	return page, pageLimit, nil
}

func scanSequenceRow(rows interface{ Scan(...any) error }) (sequenceRow, int, error) {
	var r sequenceRow
	var last *int64
	var pct *float64
	var limit int64
	s := &r.stats
	if err := rows.Scan(&r.oid, &s.SchemaName, &s.SequenceName, &s.DataType, &r.readable,
		&last, &s.MinValue, &s.MaxValue, &s.IncrementBy, &s.Cycle, &pct, &limit); err != nil {
		return r, 0, err
	}
	if last != nil && pct != nil {
		r.used, s.LastValue, s.PctUsed = true, *last, *pct
	}
	return r, int(limit), nil
}

func positiveOr(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}
