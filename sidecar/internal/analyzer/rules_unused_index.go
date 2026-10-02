package analyzer

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ruleUnusedIndexes flags indexes with zero scans that are not primary keys,
// not unique, and have had zero scans for a full window of clean evidence.
// The evidence drives an autonomous DROP INDEX, so a window that contains a
// statistics reset is broken evidence, not zero scans: the unused clock
// restarts at the reset (G2-B07, snapshot dedupe follow-up).
func ruleUnusedIndexes(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	extras *RuleExtras,
) []Finding {
	window := time.Duration(cfg.Analyzer.UnusedIndexWindowDays) * 24 * time.Hour
	now := extras.now()
	epoch := relationStatsEpoch(current, extras)
	prev := previousIndexes(previous)
	unlogged := buildUnloggedSet(current)
	fkRequirements := buildFKRequirements(current)
	var findings []Finding

	for _, idx := range current.Indexes {
		ident := idx.SchemaName + "." + idx.IndexRelName
		if idx.IdxScan > 0 {
			// Used since the stats epoch: restart the observation window so
			// a later pg_stat_reset/crash does not look like weeks of disuse.
			extras.forgetUnused(ident)
			continue
		}
		if isSystemSchema(idx.SchemaName) ||
			idx.IsPrimary || idx.IsUnique || !idx.IsValid {
			continue
		}
		// Skip indexes recently created by the executor.
		if _, ok := extras.RecentlyCreated[idx.IndexRelName]; ok {
			continue
		}
		if indexIsOnlyFKSupport(idx, current.Indexes, fkRequirements) {
			continue
		}
		since := unusedSince(extras, idx, ident, prev[ident], epoch, now)
		if now.Sub(since) < window {
			continue
		}
		f := unusedIndexFinding(idx, ident, unlogged, cfg.Analyzer.UnusedIndexWindowDays)
		f.Detail["unused_since"] = since.UTC().Format(time.RFC3339)
		if !epoch.IsZero() {
			f.Detail["stats_epoch"] = epoch.UTC().Format(time.RFC3339)
		}
		findings = append(findings, f)
	}
	return findings
}

// relationStatsEpoch is the later of the epoch the snapshot recorded and
// the one the analyzer read live: either alone proves a reset.
func relationStatsEpoch(current *collector.Snapshot, extras *RuleExtras) time.Time {
	epoch := extras.StatsEpoch
	if current.System.RelationStatsEpoch.After(epoch) {
		epoch = current.System.RelationStatsEpoch
	}
	return epoch
}

// previousIndexes indexes the previous snapshot's indexes by identity.
func previousIndexes(previous *collector.Snapshot) map[string]collector.IndexStats {
	out := map[string]collector.IndexStats{}
	if previous == nil {
		return out
	}
	for _, idx := range previous.Indexes {
		out[idx.SchemaName+"."+idx.IndexRelName] = idx
	}
	return out
}

// unusedSince returns the start of the index's clean zero-scan window and
// records the first-seen part of it. The clock starts at the first
// zero-scan sample of this object; it restarts now for a new object under
// the name (another oid) and for a counter that went down since the
// previous sample; and the window never starts before the relation stats
// epoch, the last reset.
func unusedSince(
	extras *RuleExtras, idx collector.IndexStats, ident string,
	prev collector.IndexStats, epoch, now time.Time,
) time.Time {
	if extras.IndexOID == nil {
		extras.IndexOID = map[string]uint32{}
	}
	since, seen := extras.FirstSeen[ident]
	known := extras.IndexOID[ident]
	newObject := idx.IndexRelID != 0 && known != 0 && known != idx.IndexRelID
	sameObject := prev.IndexRelID == idx.IndexRelID
	decreased := sameObject && prev.IndexRelName != "" && prev.IdxScan > idx.IdxScan
	if !seen || newObject || decreased {
		since = now
	}
	extras.FirstSeen[ident] = since
	if idx.IndexRelID != 0 {
		extras.IndexOID[ident] = idx.IndexRelID
	}
	if epoch.After(since) {
		return epoch // re-read every cycle; a reset is never forgotten
	}
	return since
}

// forgetUnused drops an index's unused clock: it was used.
func (e *RuleExtras) forgetUnused(ident string) {
	delete(e.FirstSeen, ident)
	delete(e.IndexOID, ident)
}

// now returns the rule clock (tests set Now).
func (e *RuleExtras) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func unusedIndexFinding(
	idx collector.IndexStats, ident string,
	unlogged map[string]bool, windowDays int,
) Finding {
	severity := "warning"
	rec := "Drop unused index to save disk and write overhead."
	detail := map[string]any{
		"table":     idx.RelName,
		"index_def": idx.IndexDef,
		"size":      idx.IndexBytes,
	}
	if unlogged[idx.SchemaName+"."+idx.RelName] {
		severity = "info"
		detail["unlogged"] = true
		rec += " (unlogged table — indexes lost on crash)"
	}
	return Finding{
		Category:         "unused_index",
		Severity:         severity,
		ObjectType:       "index",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf(
			"Unused index %s (0 scans for %d+ days)", ident, windowDays,
		),
		Detail:         detail,
		Recommendation: rec,
		RecommendedSQL: dropIndexSQL(idx),
		RollbackSQL:    idx.IndexDef + ";",
		ActionRisk:     "safe",
	}
}
