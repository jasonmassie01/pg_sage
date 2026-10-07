package analyzer

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// History in the meta database (history.store: meta): the footprint guard
// keeps measuring the monitored database's sage schema, where history no
// longer grows, and says so; the storage self-budget adds this database's
// share of the store, because that is still storage pg_sage uses for it.

// historyStorageEvery bounds how often the store share is measured (an
// index-only count of this database's rows per history table).
const historyStorageEvery = 15 * time.Minute

type historyStorageCache struct {
	mu    sync.Mutex
	at    time.Time
	bytes int64
}

// historyStorage is this database's share of the history store in bytes;
// 0 when its history is in its own sage schema (already measured).
func (a *Analyzer) historyStorage(ctx context.Context) (int64, error) {
	st := histstore.Resolve(a.pool)
	if !st.Scoped() {
		return 0, nil
	}
	c := &a.historyBytes
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < historyStorageEvery {
		return c.bytes, nil
	}
	b, err := st.HistoryBytes(ctx)
	if err != nil {
		return 0, err
	}
	c.at, c.bytes = time.Now(), b
	return b, nil
}

// withHistoryStorage adds the store share to the measured storage. An
// unknown share makes storage unknown: it proves nothing either way.
func withHistoryStorage(u selfbudget.Usage, bytes int64, err error) selfbudget.Usage {
	if !u.StorageKnown {
		return u
	}
	if err != nil {
		u.StorageKnown = false
		return u
	}
	u.StorageBytes += bytes
	return u
}

// annotateHistoryPlacement tells a footprint finding that history lives
// in the meta database, and that history tables still listed here are
// rows left from before the migration.
func annotateHistoryPlacement(findings []Finding, mode histstore.Mode) []Finding {
	if mode != histstore.ModeMeta {
		return findings
	}
	for i, f := range findings {
		if f.Category != categorySageFootprint {
			continue
		}
		if f.Detail == nil {
			f.Detail = map[string]any{}
		}
		f.Detail["history_store"] = string(histstore.ModeMeta)
		f.Recommendation = footprintMetaAdvice(f.Detail["largest_tables"])
		findings[i] = f
	}
	return findings
}

func footprintMetaAdvice(largest any) string {
	advice := "Telemetry history (snapshots and the query store) is kept in the meta " +
		"database (history.store: meta), so it no longer grows here. "
	if listsHistory(largest) {
		advice += "The sage.snapshots or sage.query_store rows listed were left from " +
			"before the migration: remove them with the sidecar's `history migrate " +
			"--cleanup` command once the copy is complete. "
	}
	return advice + "Otherwise shorten the retention of the largest table listed, or " +
		"raise retention.sage_size_warning_pct if this share is expected."
}

// listsHistory reports a history table (or one of its partitions) among
// the finding's largest tables.
func listsHistory(largest any) bool {
	rows, _ := largest.([]map[string]any)
	for _, r := range rows {
		name, _ := r["table"].(string)
		if strings.HasPrefix(name, "sage.snapshots") ||
			strings.HasPrefix(name, "sage.query_store") {
			return true
		}
	}
	return false
}
