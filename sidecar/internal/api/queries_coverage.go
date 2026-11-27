package api

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// GET /api/v1/queries/coverage (evid-01b): per database, how much of the
// execution time the long per-query history (the hourly and daily rollup)
// speaks for over a window, its epoch breaks and gaps, and the rollup's
// size. A database without rollup rows answers status unknown, never ok.

// Bounds of a coverage request: the window defaults to the last 28 days
// and reads at most the daily tier's default retention.
const (
	defaultCoverageWindow = 28 * 24 * time.Hour
	maxCoverageWindow     = 400 * 24 * time.Hour
)

// coverageEntry is one database's coverage report.
type coverageEntry struct {
	Database          string   `json:"database"`
	Status            string   `json:"status"`
	CoveredExecShare  *float64 `json:"covered_exec_share"`
	EpochBreaks       int      `json:"epoch_breaks"`
	Hours             int      `json:"hours"`
	HoursMissing      int      `json:"hours_missing"`
	DeallocGrowth     *int64   `json:"dealloc_growth"`
	CountersOnlyHours int      `json:"epoch_source_counters_only_hours"`
	OldestHour        *string  `json:"oldest_hour"`
	OldestDay         *string  `json:"oldest_day"`
	RollupBytes       int64    `json:"rollup_bytes"`
}

// parseCoverageWindow reads from and to (RFC 3339): to defaults to now
// and from to 28 days before it; from must be before to, at most 400 days.
func parseCoverageWindow(q url.Values, now time.Time) (time.Time, time.Time, error) {
	to, err := timeParam(q, "to", now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	from, err := timeParam(q, "from", to.Add(-defaultCoverageWindow))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("from (%s) must be before to (%s)",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if to.Sub(from) > maxCoverageWindow {
		return time.Time{}, time.Time{}, fmt.Errorf("the window is %v; at most %v is read",
			to.Sub(from), maxCoverageWindow)
	}
	return from, to, nil
}

func formatTime(t *time.Time, layout string) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(layout)
	return &s
}

func toCoverageEntry(db string, r querystore.CoverageReport) coverageEntry {
	e := coverageEntry{Database: db, Status: r.Status, EpochBreaks: r.EpochBreaks,
		Hours: r.Hours, HoursMissing: r.HoursMissing, DeallocGrowth: r.DeallocGrowth,
		CountersOnlyHours: r.CountersOnlyHours, RollupBytes: r.RollupBytes,
		OldestHour: formatTime(r.OldestHour, time.RFC3339),
		OldestDay:  formatTime(r.OldestDay, time.DateOnly)}
	if r.CoveredExecShare != nil {
		v := round6(*r.CoveredExecShare)
		e.CoveredExecShare = &v
	}
	return e
}

func queriesCoverageHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		from, to, err := parseCoverageWindow(r.URL.Query(), time.Now().UTC())
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if database == "" {
			database = "all"
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		entries := []coverageEntry{}
		for _, sel := range poolsForDatabaseSelection(mgr, database) {
			rep, err := querystore.RollupCoverage(r.Context(), sel.pool, from, to)
			if err != nil {
				internalError(w, r, "read the query rollup coverage of "+sel.name, err)
				return
			}
			entries = append(entries, toCoverageEntry(sel.name, rep))
		}
		jsonResponse(w, map[string]any{"database": responseDatabaseName(database),
			"since": from, "until": to, "databases": entries})
	}
}
