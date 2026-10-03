package collector

import (
	"cmp"
	"slices"
	"time"
)

// Caches that keep the collector's catalog reads O(changes) instead of
// O(catalog) per cycle (perf fix phase, measured.md M1/M3/M4/M7). They are
// touched only by the collection cycle, under Collector.catalogMu.

// selectSequences keeps the sequences that matter, as the v1.8.2 SQL did:
// every one at or above floorPct of its range plus the topN most used,
// ordered by use (ties by schema, then name), at most maxRows. The input
// is not modified.
func selectSequences(all []SequenceStats, floorPct float64, topN, maxRows int) []SequenceStats {
	sorted := slices.Clone(all)
	slices.SortFunc(sorted, func(a, b SequenceStats) int {
		if c := cmp.Compare(b.PctUsed, a.PctUsed); c != 0 {
			return c
		}
		if c := cmp.Compare(a.SchemaName, b.SchemaName); c != 0 {
			return c
		}
		return cmp.Compare(a.SequenceName, b.SequenceName)
	})
	out := make([]SequenceStats, 0, min(len(sorted), max(maxRows, 0)))
	for i, s := range sorted {
		if len(out) >= maxRows {
			break
		}
		if s.PctUsed >= floorPct || i < topN {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil // as the SQL scan did: "no sequences" persists as null
	}
	return out
}

// sequenceRow is one sequence of a catalog page: used means its last value
// was read (readable and called at least once).
type sequenceRow struct {
	oid      uint32
	readable bool
	used     bool
	stats    SequenceStats
}

// sequenceCache holds the last read of every used sequence by oid, so a
// cycle that reads only part of the catalog (the scan cap) still reports
// all of it. cursor is where the next cycle resumes (0: from the start).
type sequenceCache struct {
	rows   map[uint32]SequenceStats
	cursor uint32
}

// apply replaces the cached range covered by one page: oids in
// (after, last oid of the page], or everything after `after` when the page
// is the catalog's last.
func (s *sequenceCache) apply(after uint32, page []sequenceRow, final bool) {
	if s.rows == nil {
		s.rows = map[uint32]SequenceStats{}
	}
	upTo := uint32(0)
	if len(page) > 0 {
		upTo = page[len(page)-1].oid
	}
	for oid := range s.rows {
		if oid > after && (final || oid <= upTo) {
			delete(s.rows, oid)
		}
	}
	for _, r := range page {
		if r.used {
			s.rows[r.oid] = r.stats
		}
	}
}

// list returns the cached sequences in oid order.
func (s *sequenceCache) list() []SequenceStats {
	oids := make([]uint32, 0, len(s.rows))
	for oid := range s.rows {
		oids = append(oids, oid)
	}
	slices.Sort(oids)
	out := make([]SequenceStats, len(oids))
	for i, oid := range oids {
		out[i] = s.rows[oid]
	}
	return out
}

// indexDefEntry is a cached pg_get_indexdef result and the catalog version
// it was read at (index row xmin and relfilenode, table and schema xmin).
type indexDefEntry struct {
	version string
	def     string
}

// indexDefCache keeps index definitions between cycles. A column rename
// changes neither pg_class row, so the whole cache is also dropped when
// the watermark (update counters of pg_attribute, pg_namespace, pg_proc)
// moves, and after maxAge as a backstop when counters are off.
type indexDefCache struct {
	entries   map[uint32]indexDefEntry
	seen      map[uint32]bool
	watermark int64
	validAt   time.Time
	primed    bool
}

func newIndexDefCache() *indexDefCache {
	return &indexDefCache{entries: map[uint32]indexDefEntry{}, seen: map[uint32]bool{}}
}

// beginPass starts a full pass over the indexes and reports whether the
// cache was flushed.
func (d *indexDefCache) beginPass(watermark int64, now time.Time, maxAge time.Duration) bool {
	d.seen = map[uint32]bool{}
	if d.primed && watermark == d.watermark && now.Sub(d.validAt) <= maxAge {
		return false
	}
	d.entries = map[uint32]indexDefEntry{}
	d.watermark, d.validAt, d.primed = watermark, now, true
	return true
}

// lookup returns the cached definition when it was read at version.
func (d *indexDefCache) lookup(oid uint32, version string) (string, bool) {
	d.seen[oid] = true
	e, ok := d.entries[oid]
	if !ok || e.version != version {
		return "", false
	}
	return e.def, true
}

func (d *indexDefCache) store(oid uint32, version, def string) {
	d.seen[oid] = true
	d.entries[oid] = indexDefEntry{version: version, def: def}
}

// endPass evicts the indexes a completed pass did not see (dropped).
func (d *indexDefCache) endPass() {
	for oid := range d.entries {
		if !d.seen[oid] {
			delete(d.entries, oid)
		}
	}
}

func (d *indexDefCache) size() int { return len(d.entries) }

// topByBytes returns the oids of the n largest relations (ties: lower oid
// first): the only ones whose exact size is measured each cycle.
func topByBytes(oids []uint32, bytes []int64, n int) []uint32 {
	if n <= 0 || len(oids) == 0 {
		return nil
	}
	idx := make([]int, len(oids))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int {
		if c := cmp.Compare(bytes[b], bytes[a]); c != 0 {
			return c
		}
		return cmp.Compare(oids[a], oids[b])
	})
	out := make([]uint32, 0, min(n, len(idx)))
	for _, i := range idx[:min(n, len(idx))] {
		out = append(out, oids[i])
	}
	slices.Sort(out)
	return out
}

// dbSizeCache is the last pg_database_size reading and when it was tried.
type dbSizeCache struct {
	bytes int64
	at    time.Time
}

func (d dbSizeCache) due(now time.Time, every time.Duration) bool {
	return d.at.IsZero() || now.Sub(d.at) >= every
}
