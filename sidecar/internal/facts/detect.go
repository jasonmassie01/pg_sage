package facts

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Detector proposes facts from deterministic evidence. Detectors only
// propose; an operator confirms.
type Detector interface {
	Name() string
	Detect(context.Context) ([]Proposal, error)
}

// appManagedDetector proposes that the application's migrations own an
// index pg_sage dropped that came back with the same definition (the
// lifeos index fight: one index dropped 8 times in 40 minutes).
type appManagedDetector struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewAppManagedDetector proposes app-owned indexes from pg_sage's own drop
// history.
func NewAppManagedDetector(pool *pgxpool.Pool) Detector {
	return appManagedDetector{pool: pool, now: time.Now}
}

func (appManagedDetector) Name() string { return "app_managed_index" }

func (d appManagedDetector) Detect(ctx context.Context) ([]Proposal, error) {
	managed, err := analyzer.AppManagedIndexes(ctx,
		catalogread.New(d.pool, catalogread.Default()))
	if err != nil {
		return nil, fmt.Errorf("app-managed indexes: %w", err)
	}
	keys := make([]string, 0, len(managed))
	for k := range managed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Proposal, 0, len(keys))
	for _, key := range keys {
		ref, err := ParseObjectRef(key)
		if err != nil || ref.Schema == "" {
			continue // an unqualified drop names no schema to own
		}
		m := managed[key]
		subject := ref.String()
		out = append(out, Proposal{Type: TypeAppMigrations, Kind: KindIndex,
			Subject: subject, Source: SourceDetector, ProposedBy: "detector:app_managed_index",
			Rationale: "pg_sage dropped this index and something recreated it with the " +
				"same definition: most likely the application's schema migrations.",
			Evidence: []Citation{{Kind: "action_log", Ref: "drop_index:" + subject,
				Detail: fmt.Sprintf("pg_sage dropped it %d times (last at %s) and it came "+
					"back with the same definition each time", m.Drops,
					m.LastDrop.UTC().Format(time.RFC3339)), ObservedAt: d.now()}}})
	}
	return out, nil
}

// slotConsumerDetector proposes the consumer of each logical slot of the
// database (CDC tools such as Debezium, Fivetran or Airbyte).
type slotConsumerDetector struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewSlotConsumerDetector proposes logical slots' consumers.
func NewSlotConsumerDetector(pool *pgxpool.Pool) Detector {
	return slotConsumerDetector{pool: pool, now: time.Now}
}

func (slotConsumerDetector) Name() string { return "slot_consumer" }

const logicalSlotsSQL = `/* pg_sage */
SELECT s.slot_name::text, COALESCE(s.plugin::text, ''), s.active,
       COALESCE(r.application_name, '')
FROM pg_catalog.pg_replication_slots s
LEFT JOIN pg_catalog.pg_stat_replication r ON r.pid = s.active_pid
WHERE s.slot_type = 'logical' AND s.database = current_database()
ORDER BY s.slot_name`

func (d slotConsumerDetector) Detect(ctx context.Context) ([]Proposal, error) {
	rows, err := d.pool.Query(ctx, logicalSlotsSQL)
	if err != nil {
		return nil, fmt.Errorf("read logical slots: %w", err)
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		var name, plugin, app string
		var active bool
		if err := rows.Scan(&name, &plugin, &active, &app); err != nil {
			return nil, fmt.Errorf("scan logical slot: %w", err)
		}
		out = append(out, slotProposal(name, plugin, app, active, d.now()))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read logical slots: %w", err)
	}
	return out, nil
}

func slotProposal(name, plugin, app string, active bool, now time.Time) Proposal {
	consumer, state := plugin, "inactive"
	if active {
		state = "active"
		if app != "" {
			consumer = app
		}
	}
	if consumer == "" {
		consumer = "unknown logical consumer"
	}
	return Proposal{Type: TypeSlotConsumer, Kind: KindSlot, Subject: name,
		Value: map[string]string{"consumer": consumer}, Source: SourceDetector,
		ProposedBy: "detector:slot_consumer",
		Rationale: "A logical slot feeds a change-data-capture consumer; dropping or " +
			"advancing it loses that consumer's changes.",
		Evidence: []Citation{{Kind: "slot", Ref: "slot:" + name, ObservedAt: now,
			Detail: fmt.Sprintf("logical slot, plugin %s, %s, application_name %q",
				plugin, state, app)}}}
}

// appendOnlyDetector proposes tables that only ever received inserts.
type appendOnlyDetector struct {
	pool       *pgxpool.Pool
	minInserts int64
	now        func() time.Time
}

// NewAppendOnlyDetector proposes tables with at least minInserts inserts
// and no update or delete since statistics were reset.
func NewAppendOnlyDetector(pool *pgxpool.Pool, minInserts int64) Detector {
	return appendOnlyDetector{pool: pool, minInserts: minInserts, now: time.Now}
}

func (appendOnlyDetector) Name() string { return "append_only" }

const insertOnlySQL = `/* pg_sage */
SELECT schemaname::text, relname::text, n_tup_ins
FROM pg_catalog.pg_stat_user_tables
WHERE n_tup_upd = 0 AND n_tup_del = 0 AND n_tup_hot_upd = 0 AND n_tup_ins >= $1
  AND schemaname <> 'sage' AND schemaname !~ '^pg_'
ORDER BY n_tup_ins DESC, schemaname, relname
LIMIT 20`

func (d appendOnlyDetector) Detect(ctx context.Context) ([]Proposal, error) {
	rows, err := d.pool.Query(ctx, insertOnlySQL, d.minInserts)
	if err != nil {
		return nil, fmt.Errorf("read insert-only tables: %w", err)
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		var schema, table string
		var inserts int64
		if err := rows.Scan(&schema, &table, &inserts); err != nil {
			return nil, fmt.Errorf("scan insert-only table: %w", err)
		}
		subject := quoteIdent(schema) + "." + quoteIdent(table)
		out = append(out, Proposal{Type: TypeAppendOnly, Kind: KindTable,
			Subject: subject, Source: SourceDetector, ProposedBy: "detector:append_only",
			Rationale: "Only inserts reach this table: an archive or a log.",
			Evidence: []Citation{{Kind: "stats", Ref: "pg_stat_user_tables:" + subject,
				Detail: fmt.Sprintf("%d inserts, no updates or deletes since statistics "+
					"were reset", inserts), ObservedAt: d.now()}}})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read insert-only tables: %w", err)
	}
	return out, nil
}
