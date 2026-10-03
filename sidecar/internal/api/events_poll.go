package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// lastSeen is a resource's change mark on a database, the poller's diff
// key: writes is the tables' write counter from the statistics system
// (rows inserted + updated + deleted; -1 when a table does not exist yet)
// and maxID their newest row id, read from the primary key index.
type lastSeen struct {
	writes int64
	maxID  int64
}

// changeSignature maps each resource to its change mark.
type changeSignature map[EventType]lastSeen

// tableWritesSQL is the write counter of one relation from the statistics
// system: constant work, no table access.
func tableWritesSQL(rel string) string {
	return fmt.Sprintf("(pg_stat_get_tuples_inserted(%[1]s) + "+
		"pg_stat_get_tuples_updated(%[1]s) + pg_stat_get_tuples_deleted(%[1]s))", rel)
}

// maxIDSQL is a table's newest id: one backward step of its primary key.
func maxIDSQL(table string) string {
	return "COALESCE((SELECT max(id) FROM sage." + table + "), 0)"
}

// changeSignatureSQL reads the three resources' change marks in one round
// trip. It replaced count(*)/max() aggregates over whole history tables
// (perf gate: ~24M rows read per 90 s with a dashboard open). New rows
// show at once through the primary keys; updates and deletes show when
// the writer's backend flushes its statistics (about a second, at most
// ~10 s on PostgreSQL 15+). The dashboard also refreshes every 30 s.
var changeSignatureSQL = `/* pg_sage */ WITH t AS (
  SELECT to_regclass('sage.findings') AS f, to_regclass('sage.action_log') AS l,
         to_regclass('sage.action_queue') AS q, to_regclass('sage.health_history') AS h)
SELECT COALESCE(` + tableWritesSQL("t.f") + `, -1)::int8, ` + maxIDSQL("findings") + `::int8,
       COALESCE(` + tableWritesSQL("t.l") + ` + ` + tableWritesSQL("t.q") + `, -1)::int8,
       (` + maxIDSQL("action_log") + ` + ` + maxIDSQL("action_queue") + `)::int8,
       COALESCE(` + tableWritesSQL("t.h") + `, -1)::int8, ` + maxIDSQL("health_history") +
	`::int8
  FROM t`

// eventResources is the publish order of the polled resources.
var eventResources = []EventType{EventFindings, EventActions, EventHealth}

func readChangeSignature(ctx context.Context, pool *pgxpool.Pool) (changeSignature, error) {
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var f, a, h lastSeen
	if err := pool.QueryRow(qctx, changeSignatureSQL).Scan(&f.writes, &f.maxID,
		&a.writes, &a.maxID, &h.writes, &h.maxID); err != nil {
		return nil, fmt.Errorf("read live-update change marks: %w", err)
	}
	return changeSignature{EventFindings: f, EventActions: a, EventHealth: h}, nil
}

func (b *EventBroker) pollLoop(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	interval time.Duration,
) {
	t := time.NewTicker(interval)
	defer t.Stop()

	// {database -> resource -> lastSeen}
	state := map[string]map[EventType]lastSeen{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-t.C:
			b.pollIfWatched(ctx, mgr, state)
		}
	}
}

// pollIfWatched polls only while a dashboard is subscribed: with nobody
// watching, the result reaches no one and the database is not touched at
// all. It reports whether it polled. A subscriber that arrives later sees
// one refresh event at most, for whatever changed while nobody watched.
func (b *EventBroker) pollIfWatched(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	state map[string]map[EventType]lastSeen,
) bool {
	if b.SubscriberCount() == 0 {
		return false
	}
	b.pollOnce(ctx, mgr, state)
	return true
}

// pollOnce reads every database's change counters (one constant-cost
// statement each) and publishes the resources that changed.
func (b *EventBroker) pollOnce(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	state map[string]map[EventType]lastSeen,
) {
	if mgr == nil {
		return
	}
	for name, inst := range mgr.Instances() {
		if inst == nil || inst.Pool == nil {
			continue
		}
		sig, err := readChangeSignature(ctx, inst.Pool)
		if err != nil {
			// Expected while the pool is being torn down; not worth a
			// warning every two seconds.
			slog.Debug("live-update poll", "database", name, "error", err)
			continue
		}
		if _, ok := state[name]; !ok {
			state[name] = map[EventType]lastSeen{}
		}
		b.applyChangeSignature(name, state[name], sig)
	}
}

// applyChangeSignature records sig and publishes one event per resource
// whose mark moved. The first observation is the baseline (no event:
// every client would refetch on connect); a missing table is skipped and
// keeps its last mark. A counter that went backwards (statistics
// reset) counts as a change.
func (b *EventBroker) applyChangeSignature(
	database string, dbState map[EventType]lastSeen, sig changeSignature,
) {
	for _, typ := range eventResources {
		mark, ok := sig[typ]
		if !ok || mark.writes < 0 {
			continue
		}
		prev, seen := dbState[typ]
		dbState[typ] = mark
		if !seen || prev == mark {
			continue
		}
		b.Publish(Event{
			Type:     typ,
			Database: database,
			Payload:  map[string]any{"changes": mark.writes, "max_id": mark.maxID},
		})
	}
}
