package analyzer

import "github.com/pg-sage/sidecar/internal/collector"

// rules-10: the seq-scan watchdog judges table scan counters over the
// interval between two snapshots. The interval is usable only when both
// snapshots read the tables, belong to the same relation stats epoch
// (pg_stat_reset, a single-relation reset and a restart all move it) and
// are in collection order.

// scanCounts is one table's seq and index scans over the interval.
type scanCounts struct {
	seq, idx int64
}

// hasRelationInterval reports whether current and previous give a usable
// table-counter interval. A zero (unknown) epoch never matches.
func hasRelationInterval(current, previous *collector.Snapshot) bool {
	if current == nil || previous == nil ||
		!current.Available("tables") || !previous.Available("tables") ||
		len(previous.Tables) == 0 {
		return false
	}
	before, now := previous.System.RelationStatsEpoch, current.System.RelationStatsEpoch
	return !before.IsZero() && !now.IsZero() && before.Equal(now) &&
		current.CollectedAt.After(previous.CollectedAt)
}

func tablesByIdent(snap *collector.Snapshot) map[string]collector.TableStats {
	out := make(map[string]collector.TableStats, len(snap.Tables))
	for _, t := range snap.Tables {
		out[t.SchemaName+"."+t.RelName] = t
	}
	return out
}

// scanDelta is what the table scanned between prev and cur; ok is false
// when a counter went down (its counters were reset), which never yields
// a negative interval.
func scanDelta(prev, cur collector.TableStats) (scanCounts, bool) {
	d := scanCounts{seq: cur.SeqScan - prev.SeqScan, idx: cur.IdxScan - prev.IdxScan}
	return d, d.seq >= 0 && d.idx >= 0
}

// seqScanUndecided names the tables with no usable interval this cycle
// (absent from the previous snapshot, or with a counter that went down):
// their absence from the findings is no verdict, so an open finding stays
// open. Nil when there is no interval at all (the category then fails).
func seqScanUndecided(current, previous *collector.Snapshot) []string {
	if !hasRelationInterval(current, previous) {
		return nil
	}
	before := tablesByIdent(previous)
	var out []string
	for _, t := range current.Tables {
		ident := t.SchemaName + "." + t.RelName
		p, seen := before[ident]
		if !seen {
			out = append(out, ident)
			continue
		}
		if _, ok := scanDelta(p, t); !ok {
			out = append(out, ident)
		}
	}
	return out
}
