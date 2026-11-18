package api

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/review/trigger"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// review-04a: the request-driven review triggers. An upgrade readiness
// check publishes pre_upgrade; a live migration lint publishes the tables
// it measured, and the database's queue keeps them as large_migration when
// one is larger than review.large_table_bytes. A failed publish is logged
// here and counted by the trigger package; the route answers as before.

type upgradeTriggerFunc func(ctx context.Context, database string,
	t upgradecheck.Target) error

type lintTriggerFunc func(ctx context.Context, database string,
	tables []trigger.TableSize) error

// publishTimeout bounds a publish so a slow queue never delays an answer.
const publishTimeout = 5 * time.Second

func fleetUpgradeTrigger(mgr *fleet.DatabaseManager) upgradeTriggerFunc {
	return func(ctx context.Context, database string, t upgradecheck.Target) error {
		name, err := resolveFleetDatabase(mgr, database)
		if err != nil {
			return err
		}
		return trigger.Default.PreUpgrade(ctx, name, t.Major, t.Method, time.Now())
	}
}

func fleetLintTrigger(mgr *fleet.DatabaseManager) lintTriggerFunc {
	return func(ctx context.Context, database string, tables []trigger.TableSize) error {
		name, err := resolveFleetDatabase(mgr, database)
		if err != nil {
			return err
		}
		return trigger.Default.LargeMigration(ctx, name, tables, time.Now())
	}
}

// publishTrigger runs fn on a context of its own (the request's may end
// as the answer is written) and logs a failure with the route and
// database.
func publishTrigger(parent context.Context, route, database string,
	fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), publishTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		slog.Warn("review trigger not published; the route answered anyway",
			"route", route, "database", database, "error", err)
	}
}

// lintTables is each table a migration review measured, once, at its
// largest measured size.
func lintTables(rep migration.Report) []trigger.TableSize {
	largest := map[string]int64{}
	for _, st := range rep.Statements {
		for _, h := range st.Hazards {
			if h.Table == "" || !h.SizeKnown {
				continue
			}
			if b, ok := largest[h.Table]; !ok || h.TableSizeBytes > b {
				largest[h.Table] = h.TableSizeBytes
			}
		}
	}
	out := make([]trigger.TableSize, 0, len(largest))
	for t, b := range largest {
		out = append(out, trigger.TableSize{Table: t, Bytes: b})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out
}
