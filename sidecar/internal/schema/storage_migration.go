package schema

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/partition"
)

// Perf storage phase (reviews/2026-10-03-perf-storage-report.md): how
// pg_sage's own data grows and is cleaned up.
//
//   - sage.query_store and sage.snapshots are partitioned by UTC day
//     (partition.Convert, in place: the old table becomes the history
//     partition), so retention drops days instead of deleting rows.
//   - No findings index keys a column the analyzer refreshes every cycle
//     (last_seen was in three), so refreshes are HOT; resolved findings age
//     on resolved_at; explain_results ages on expires_at.
//   - Hot-update tables keep room for HOT updates (fillfactor) and small
//     hot tables are vacuumed at a few dead rows, not 50 + 20%.
//
// Every step checks the catalog first, so a second bootstrap writes
// nothing. New indexes are plain CREATE INDEX (bootstrap holds its own
// advisory lock; the tables are pg_sage's and small): never on
// sage.decision, which only gets a storage parameter.

// storageMigrationTimeout bounds the migration. Startup bounds bootstrap
// tighter (10 s): only small tables are converted here.
const storageMigrationTimeout = 2 * time.Minute

// bootstrapConvertMaxBytes is the largest history table heap bootstrap
// converts (validating it reads the heap once: well under a second at this
// size). Larger tables are converted by retention in the background
// (retention.Cleaner.ConvertHistory), off the startup path.
var bootstrapConvertMaxBytes int64 = 32 << 20

// partitionDaysAhead is how many days of partitions bootstrap ensures.
const partitionDaysAhead = 4

// storageIndex is an index the migration creates when pg_indexes lacks it.
type storageIndex struct{ name, ddl string }

var storageIndexes = []storageIndex{
	{"idx_findings_status_severity",
		"CREATE INDEX idx_findings_status_severity ON sage.findings (status, severity)"},
	{"idx_findings_schema_lint_open", `CREATE INDEX idx_findings_schema_lint_open
		ON sage.findings (category, severity)
		WHERE category LIKE 'schema_lint:%' AND status = 'open'`},
	{"idx_findings_resolved_at", `CREATE INDEX idx_findings_resolved_at
		ON sage.findings (resolved_at) WHERE status = 'resolved'`},
	{"idx_explain_results_expires",
		"CREATE INDEX idx_explain_results_expires ON sage.explain_results (expires_at)"},
	// The action queue purge keeps rows an SRE proposal points at.
	{"idx_sre_action_proposals_queue", `CREATE INDEX idx_sre_action_proposals_queue
		ON sage.sre_action_proposals (queue_id) WHERE queue_id IS NOT NULL`},
}

// retiredIndexes keyed last_seen, which every findings refresh updates.
var retiredIndexes = []string{"idx_findings_status", "idx_findings_schema_lint",
	"idx_findings_resolved_last_seen"}

// storageSetting is a table's storage parameters, as reloptions spells them.
type storageSetting struct {
	table string
	opts  []string
}

var smallHot = []string{"autovacuum_vacuum_threshold=10",
	"autovacuum_vacuum_scale_factor=0.05"}

var storageSettings = []storageSetting{
	{"findings", []string{"fillfactor=80"}},
	{"recommendation", []string{"fillfactor=90"}},
	{"decision", []string{"fillfactor=90"}},
	{"incidents", append([]string{"fillfactor=80"}, smallHot...)},
	{"sre_change_feed_state", append([]string{"fillfactor=50"}, smallHot...)},
	{"sre_service_slos", append([]string{"fillfactor=50"}, smallHot...)},
	{"sre_investigations", append([]string{"fillfactor=70"}, smallHot...)},
}

// backfillResolvedAt gives resolved findings from before resolved_at was
// always set their last sighting as the resolution time.
const backfillResolvedAt = `UPDATE sage.findings SET resolved_at = last_seen
WHERE status = 'resolved' AND resolved_at IS NULL`

// migrateStorage runs the storage steps on the bootstrap session.
func migrateStorage(ctx context.Context, db partition.DB) error {
	ctx, cancel := context.WithTimeout(ctx, storageMigrationTimeout)
	defer cancel()
	for _, t := range partition.HistoryTables() {
		if err := partitionAtBootstrap(ctx, db, t); err != nil {
			return err
		}
	}
	if _, err := db.Exec(ctx, ddlSnapshotDataAt); err != nil {
		return fmt.Errorf("snapshot accessor: %w", err)
	}
	if err := migrateStorageIndexes(ctx, db); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, backfillResolvedAt); err != nil {
		return fmt.Errorf("backfill findings resolved_at: %w", err)
	}
	return migrateStorageSettings(ctx, db)
}

func migrateStorageIndexes(ctx context.Context, db partition.DB) error {
	for _, ix := range storageIndexes {
		var present bool
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_indexes
			WHERE schemaname = 'sage' AND indexname = $1)`, ix.name).Scan(&present); err != nil {
			return fmt.Errorf("check index %s: %w", ix.name, err)
		}
		if present {
			continue
		}
		if _, err := db.Exec(ctx, ix.ddl); err != nil {
			return fmt.Errorf("create index %s: %w", ix.name, err)
		}
	}
	for _, name := range retiredIndexes {
		var present bool
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_indexes
			WHERE schemaname = 'sage' AND indexname = $1)`, name).Scan(&present); err != nil {
			return fmt.Errorf("check index %s: %w", name, err)
		}
		if !present {
			continue
		}
		if _, err := db.Exec(ctx, "DROP INDEX sage."+name); err != nil {
			return fmt.Errorf("drop index %s: %w", name, err)
		}
	}
	return nil
}

func migrateStorageSettings(ctx context.Context, db partition.DB) error {
	for _, s := range storageSettings {
		var current []string
		err := db.QueryRow(ctx, `SELECT COALESCE(reloptions, '{}') FROM pg_catalog.pg_class
			WHERE oid = pg_catalog.to_regclass($1)`, "sage."+s.table).Scan(&current)
		if err != nil {
			return fmt.Errorf("read storage parameters of sage.%s: %w", s.table, err)
		}
		missing := false
		for _, o := range s.opts {
			missing = missing || !slices.Contains(current, o)
		}
		if !missing {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE sage.%s SET (%s)", s.table, strings.Join(s.opts, ", "))
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set storage parameters of sage.%s: %w", s.table, err)
		}
	}
	return nil
}

// ddlSnapshotDataAt is the accessor readers use (snapstore.DataSQL): the
// row's collection time confines each base lookup to the row's UTC day
// partition, where the writer keeps every base. A base outside it (rows
// written before day partitioning) is found by id alone.
const ddlSnapshotDataAt = `
CREATE OR REPLACE FUNCTION sage.snapshot_data(p_data jsonb, p_base_id bigint,
                                              p_at timestamptz)
RETURNS jsonb LANGUAGE plpgsql STABLE PARALLEL SAFE AS $fn$
DECLARE
    v_data    jsonb := p_data;
    v_base_id bigint := p_base_id;
    v_at      timestamptz := p_at;
    v_chain   jsonb[] := '{}';
    v_depth   int := 0;
    r         record;
BEGIN
    WHILE v_base_id IS NOT NULL LOOP
        v_depth := v_depth + 1;
        IF v_depth > 8 THEN
            RETURN NULL;
        END IF;
        v_chain := array_prepend(v_data, v_chain);
        SELECT b.data, b.base_id, b.collected_at INTO r
          FROM sage.snapshots b
         WHERE b.id = v_base_id AND v_at IS NOT NULL
           AND b.collected_at >= date_trunc('day', v_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
           AND b.collected_at <= v_at;
        IF NOT FOUND THEN
            SELECT b.data, b.base_id, b.collected_at INTO r
              FROM sage.snapshots b WHERE b.id = v_base_id;
            IF NOT FOUND THEN
                RETURN NULL; -- the base aged out or was deleted
            END IF;
        END IF;
        v_data := r.data;
        v_base_id := r.base_id;
        v_at := r.collected_at;
    END LOOP;
    FOR i IN 1 .. COALESCE(array_length(v_chain, 1), 0) LOOP
        v_data := sage.snapshot_apply(v_data, v_chain[i]);
    END LOOP;
    RETURN v_data;
END
$fn$;
`

// partitionAtBootstrap converts the plain history table t when it is small
// and ensures its days. A large table, or a conversion that fails (a busy
// table, a timeout), leaves the plain table as it was: pg_sage runs on it
// and retention converts it later, with backoff. Only a failure to read the
// catalog or to create days fails bootstrap.
func partitionAtBootstrap(ctx context.Context, db partition.DB, t partition.Table) error {
	var kind string
	var heap int64
	if err := db.QueryRow(ctx, `SELECT relkind::text, pg_catalog.pg_relation_size(oid)
		FROM pg_catalog.pg_class WHERE oid = pg_catalog.to_regclass($1)`, "sage."+t.Name).
		Scan(&kind, &heap); err != nil {
		return fmt.Errorf("read sage.%s: %w", t.Name, err)
	}
	if kind != "p" {
		if heap > bootstrapConvertMaxBytes {
			slog.Info("schema: sage."+t.Name+" will be partitioned by day in the background",
				"heap_mb", heap>>20)
			return nil
		}
		if _, err := partition.Convert(ctx, db, t); err != nil {
			slog.Warn("schema: sage."+t.Name+" stays one plain table for now: partitioning "+
				"it by day failed; pg_sage keeps working on it and retention retries later. "+
				partition.Hint(err), "error", err)
			return nil
		}
	}
	if _, err := partition.Ensure(ctx, db, t, time.Now(), partitionDaysAhead); err != nil {
		return fmt.Errorf("partitions of sage.%s: %w", t.Name, err)
	}
	return nil
}
