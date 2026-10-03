package analyzer

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ruleUnusedIndexes flags indexes that are not primary keys, not unique,
// and have gone unused for a full window of clean evidence: zero scans
// since a clock that restarts at every statistics reset (G2-B07, snapshot
// dedupe follow-up), or (PG16+) a last_idx_scan older than the window,
// which is durable evidence of its own (G-P0-12). The evidence drives an
// autonomous DROP INDEX, which is never automatic while standby index
// usage is unknown (G-P0-12).
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
	byTable := indexesByTable(current.Indexes)
	standbyRisk := standbyUsageUnknown(current)
	var findings []Finding

	for _, idx := range current.Indexes {
		ident := idx.SchemaName + "." + idx.IndexRelName
		if scannedWithin(idx, window, now) {
			// Used recently: restart the observation window so a later
			// pg_stat_reset/crash does not look like weeks of disuse.
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
		if indexIsOnlyFKSupport(idx, byTable, fkRequirements) {
			continue
		}
		since := unusedClock(extras, idx, ident, prev[ident], epoch, now)
		if now.Sub(since) < window {
			continue
		}
		f := unusedIndexFinding(idx, ident, unlogged, cfg.Analyzer.UnusedIndexWindowDays)
		f.Detail["unused_since"] = since.UTC().Format(time.RFC3339)
		if !epoch.IsZero() {
			f.Detail["stats_epoch"] = epoch.UTC().Format(time.RFC3339)
		}
		findings = append(findings, withStandbyGate(f, standbyRisk))
	}
	return findings
}

// scannedWithin reports whether the index counts as used within window:
// without last_idx_scan (before PG16) any scan since the stats epoch does.
func scannedWithin(idx collector.IndexStats, window time.Duration, now time.Time) bool {
	if idx.IdxScan == 0 {
		return false
	}
	return idx.LastIdxScan == nil || now.Sub(*idx.LastIdxScan) < window
}

// unusedClock is when the index's unused window starts. An index that was
// scanned, but last longer ago than the window, is unused since that scan:
// last_idx_scan is reset with the counters, so a non-null value is always
// after the stats epoch and needs no in-memory clock. Zero-scan indexes
// use the reset-aware clock.
func unusedClock(
	extras *RuleExtras, idx collector.IndexStats, ident string,
	prev collector.IndexStats, epoch, now time.Time,
) time.Time {
	if idx.IdxScan > 0 && idx.LastIdxScan != nil {
		extras.forgetUnused(ident)
		if epoch.After(*idx.LastIdxScan) {
			return epoch
		}
		return *idx.LastIdxScan
	}
	return unusedSince(extras, idx, ident, prev, epoch, now)
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
	title := fmt.Sprintf("Unused index %s (0 scans for %d+ days)", ident, windowDays)
	detail := map[string]any{
		"table":          idx.RelName,
		"index_def":      idx.IndexDef,
		"size":           idx.IndexBytes,
		"usage_evidence": "zero_scans",
	}
	if idx.IdxScan > 0 && idx.LastIdxScan != nil {
		detail["usage_evidence"] = "last_idx_scan"
		detail["last_idx_scan"] = idx.LastIdxScan.UTC().Format(time.RFC3339)
		detail["idx_scan"] = idx.IdxScan
		title = fmt.Sprintf("Unused index %s (not scanned for %d+ days)", ident, windowDays)
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
		Title:            title,
		Detail:           detail,
		Recommendation:   rec,
		RecommendedSQL:   dropIndexSQL(idx),
		RollbackSQL:      idx.IndexDef + ";",
		ActionRisk:       "safe",
	}
}
