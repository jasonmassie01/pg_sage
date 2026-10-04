package tuning

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// maxWriteCaseStatements bounds the statements a write case carries.
const maxWriteCaseStatements = 5

// writeTable is one workload table's write activity over the interval.
type writeTable struct {
	name         string
	stats        collector.TableStats
	indexes      []collector.IndexStats
	indexWrites  float64 // index entries written per second
	dUpdDel      int64
	unusedOrDupe string
}

// writeCases are the tables whose index maintenance or dead-tuple churn
// is a problem. Rates need an interval, so the first cycle has none.
func writeCases(cur, prev *collector.Snapshot, w Workload, stmts []intervalStmt,
	th Thresholds) []Case {
	if prev == nil {
		return nil
	}
	secs := cur.CollectedAt.Sub(prev.CollectedAt).Seconds()
	if secs <= 0 {
		return nil
	}
	tables := writeTables(cur, prev, w, secs)
	total := 0.0
	for _, t := range tables {
		total += t.indexWrites
	}
	var out []Case
	for _, t := range tables {
		reason := writeReason(t, th)
		if reason == "" {
			continue
		}
		c := Case{ID: "write_amplification:" + t.name, Kind: CaseWriteAmplification,
			Tables: []string{t.name}, Reason: reason,
			Statements: tableStatements(stmts, t.name)}
		if total > 0 {
			c.Weight = t.indexWrites / total
		}
		out = append(out, c)
	}
	return out
}

func writeTables(cur, prev *collector.Snapshot, w Workload, secs float64) []writeTable {
	before := make(map[string]collector.TableStats, len(prev.Tables))
	for _, t := range prev.Tables {
		before[qualified(t.SchemaName, t.RelName)] = t
	}
	byTable := map[string][]collector.IndexStats{}
	for _, ix := range cur.Indexes {
		k := qualified(ix.SchemaName, ix.RelName)
		byTable[k] = append(byTable[k], ix)
	}
	var out []writeTable
	for _, t := range cur.Tables {
		name := qualified(t.SchemaName, t.RelName)
		p, ok := before[name]
		info := w.Tables[name]
		if !ok || (info.Class != ClassApp && info.Class != ClassTenant) {
			continue
		}
		dIns, dUpd := t.NTupIns-p.NTupIns, t.NTupUpd-p.NTupUpd
		dHot, dDel := t.NTupHotUpd-p.NTupHotUpd, t.NTupDel-p.NTupDel
		if dIns < 0 || dUpd < 0 || dHot < 0 || dDel < 0 {
			continue // counters reset
		}
		idx := byTable[name]
		entries := float64(dIns+dUpd-min(dHot, dUpd)) * float64(len(idx))
		out = append(out, writeTable{name: name, stats: t, indexes: idx,
			indexWrites: entries / secs, dUpdDel: dUpd + dDel,
			unusedOrDupe: wastedIndex(idx)})
	}
	return out
}

// writeReason says why a table is a write case, or "" when it is not.
func writeReason(t writeTable, th Thresholds) string {
	var parts []string
	if t.indexWrites >= th.MinIndexWritesPerSec && t.unusedOrDupe != "" {
		parts = append(parts, fmt.Sprintf("%.1f index entries written per second, %s",
			t.indexWrites, t.unusedOrDupe))
	}
	live, dead := t.stats.NLiveTup, t.stats.NDeadTup
	if dead >= th.MinDeadTuples && live+dead > 0 &&
		float64(dead)/float64(live+dead) >= th.MinDeadRatio &&
		t.stats.TableBytes >= th.MinTableBytes && t.dUpdDel > 0 {
		parts = append(parts, fmt.Sprintf("%d dead tuples (%.0f%%) with %d updates and "+
			"deletes this interval", dead, 100*float64(dead)/float64(live+dead), t.dUpdDel))
	}
	return strings.Join(parts, "; ")
}

// wastedIndex names an index that costs writes for nothing: a valid,
// non-unique index never scanned, or one another valid index covers.
func wastedIndex(idx []collector.IndexStats) string {
	for _, ix := range idx {
		if !ix.IsValid || ix.IsUnique || ix.IsPrimary {
			continue
		}
		if by := coveringIndex(ix, idx); by != "" {
			return fmt.Sprintf("index %s is covered by %s", ix.IndexRelName, by)
		}
		if ix.IdxScan == 0 {
			return fmt.Sprintf("index %s was never scanned", ix.IndexRelName)
		}
	}
	return ""
}

// coveringIndex is the name of another valid index that serves every
// lookup ix does, or "".
func coveringIndex(ix collector.IndexStats, all []collector.IndexStats) string {
	for _, other := range all {
		if other.IndexRelName == ix.IndexRelName || !other.IsValid {
			continue
		}
		if optimizer.CoveredBy(ix.IndexDef, other.IndexDef) {
			return other.IndexRelName
		}
	}
	return ""
}

// tableStatements are the busiest interval statements touching table.
func tableStatements(stmts []intervalStmt, table string) []CaseStatement {
	var out []CaseStatement
	for _, s := range stmts {
		if slices.Contains(s.tables, table) && s.Calls > 0 {
			out = append(out, s.CaseStatement)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TotalMs > out[j].TotalMs })
	if len(out) > maxWriteCaseStatements {
		out = out[:maxWriteCaseStatements]
	}
	return out
}
