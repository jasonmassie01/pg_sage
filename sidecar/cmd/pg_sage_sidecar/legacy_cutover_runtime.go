package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// legacyCutoverInterval is how often a database's legacy PagerDuty pages
// are checked for a resolved finding after the cutover to notify.
const legacyCutoverInterval = time.Minute

// startLegacyCutover resolves, through the reconciled PagerDuty channel,
// the pages the legacy engine opened on this database before the switch
// to the notify engine, each under its legacy key once its finding
// resolves (alerting-06b). It stops when none is left.
func (rt *databaseRuntime) startLegacyCutover() {
	if cfg == nil || !cfg.Alerting.Enabled || rt.dispatcher == nil || rt.spec.Pool == nil ||
		cfg.Alerting.EffectiveEngine() != config.AlertingEngineNotify {
		return
	}
	store := sharedNotifyStore(rt.spec.ControlPool)
	if store == nil {
		return
	}
	pool, name, d := rt.spec.Pool, rt.spec.Name, rt.dispatcher
	rt.start(func() { runLegacyCutover(rt.ctx, pool, name, d, store) })
}

func runLegacyCutover(
	ctx context.Context, pool *pgxpool.Pool, name string, d *notify.Dispatcher,
	store *notify.PoolStore,
) {
	ticker := time.NewTicker(legacyCutoverInterval)
	defer ticker.Stop()
	for !legacyCutoverSweep(ctx, pool, name, d, store) {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// legacyCutoverSweep resolves what it can and reports whether the
// cutover is done: no legacy page is owed a resolve (in this batch or
// beyond it), no PagerDuty target
// is configured to send one, or this database has no alert log.
func legacyCutoverSweep(
	ctx context.Context, pool *pgxpool.Pool, name string, d *notify.Dispatcher,
	store *notify.PoolStore,
) bool {
	ch, err := store.ChannelByName(ctx, notify.ConfigChannelName("pagerduty", "pagerduty"))
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	if err != nil {
		logWarn("alerting", "legacy page cutover on %q: reading the PagerDuty channel: %v",
			name, err)
		return false
	}
	res, err := notify.ResolveLegacyPages(ctx, pool, name, d, *ch)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return true // no sage.alert_log here: the legacy engine never ran
	}
	if err != nil {
		logWarn("alerting", "legacy page cutover on %q: %v", name, err)
		return false
	}
	if res.Resolved > 0 || res.Failed > 0 {
		logInfo("alerting", "legacy page cutover on %q: %d resolved under the legacy key, "+
			"%d failed and retried, %d still open", name, res.Resolved, res.Failed,
			res.StillOpen)
	}
	return res.StillOpen == 0 && !res.More
}
