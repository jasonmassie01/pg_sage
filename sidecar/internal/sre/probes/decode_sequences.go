package probes

import "fmt"

// SequenceCoverage is how much of the catalog one sequence_runway run
// covered: sequences in the catalog (ascending, this session's), read
// (at most SequenceScanCap), used (with a last value) and unreadable (no
// privilege), and the rows reported. Truncated means more used
// sequences exist than were reported (nearest their limit first).
type SequenceCoverage struct {
	Total      int64
	Scanned    int64
	Used       int64
	Unreadable int64
	Reported   int64
	Truncated  bool
}

// ScanCapped reports sequences that were not read at all.
func (c SequenceCoverage) ScanCapped() bool { return c.Scanned < c.Total }

// SequenceCoverageOf decodes the coverage of a sequence_runway result.
// A result without rows (no used sequence among those read) has zero
// coverage; a failed probe is an error.
func SequenceCoverageOf(res Result) (SequenceCoverage, error) {
	rows, err := rowsFor(res, SequenceRunwayProbe)
	if err != nil || len(rows) == 0 {
		return SequenceCoverage{}, err
	}
	var c SequenceCoverage
	fields := []struct {
		key string
		dst *int64
	}{{"sequences_total", &c.Total}, {"sequences_scanned", &c.Scanned},
		{"sequences_used", &c.Used}, {"sequences_unreadable", &c.Unreadable}}
	for _, f := range fields {
		v, err := intField(rows[0], f.key)
		if err != nil {
			return SequenceCoverage{}, fmt.Errorf("sequence_runway coverage: %w", err)
		}
		*f.dst = v
	}
	if c.Scanned > c.Total || c.Used > c.Scanned || c.Unreadable > c.Scanned {
		return SequenceCoverage{}, fmt.Errorf("sequence_runway coverage is inconsistent: "+
			"%+v", c)
	}
	c.Reported, c.Truncated = int64(len(rows)), res.Truncated
	return c, nil
}
