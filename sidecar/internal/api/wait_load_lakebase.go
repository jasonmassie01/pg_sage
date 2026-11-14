package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

// The wait-load chart's Lakebase source (market-01b): the endpoint's
// wait_event_counters intervals as average active sessions by wait class.
// Preference order: Performance Insights, then Lakebase wait counters,
// then the collector's pg_stat_activity samples (always returned).

const (
	waitSourceLakebase    = "lakebase_wait_counters"
	waitLakebaseSampling  = "per counter interval (average active sessions)"
	waitLakebaseStreamTag = string(lakebaseobs.WaitCounters)
)

// LakebaseWaitLoadFunc reads a registered database's Lakebase wait load
// over [from, to]. It returns lakebaseobs.ErrHistoryUnavailable when the
// database has no Lakebase telemetry runtime.
type LakebaseWaitLoadFunc func(ctx context.Context, database string,
	from, to time.Time) (lakebaseobs.WaitLoadSeries, error)

// preferredWaitProvider is the provider breakdown to name beside the
// activity samples and its preferred_source, or nil when there is none.
func preferredWaitProvider(ctx context.Context, seam LakebaseWaitLoadFunc,
	database string, from, to time.Time) (map[string]any, string) {
	if p := providerWaitLoad(database); p != nil {
		return p, waitSourcePI
	}
	if p := lakebaseWaitLoad(ctx, seam, database, from, to); p != nil {
		return p, waitSourceLakebase
	}
	return nil, ""
}

// lakebaseWaitLoad is the Lakebase provider body, or nil when the seam is
// absent, unavailable, failing (logged) or has no interval in the window.
func lakebaseWaitLoad(ctx context.Context, seam LakebaseWaitLoadFunc,
	database string, from, to time.Time) map[string]any {
	if seam == nil {
		return nil
	}
	s, err := seam(ctx, database, from, to)
	if err != nil {
		if !errors.Is(err, lakebaseobs.ErrHistoryUnavailable) {
			slog.Warn("wait load: Lakebase wait counters unreadable; "+
				"answering from activity samples", "database", database,
				"stream", waitLakebaseStreamTag, "err", err)
		}
		return nil
	}
	if len(s.Points) == 0 {
		return nil
	}
	return map[string]any{"source": "lakebase", "provider": "lakebase",
		"sampling": waitLakebaseSampling, "collected_at": s.CurrentAt,
		"load_by_wait": s.Current, "series": s.Points, "classes": s.Classes,
		"events": s.Events, "skipped": s.Skipped, "truncated": s.Truncated,
		"wait_event_time_unit": lakebaseobs.WaitEventTimeUnitName, "unit_confirmed": false}
}
