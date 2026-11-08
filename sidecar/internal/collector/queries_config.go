package collector

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const configSettingsSQL = `/* pg_sage */
		SELECT name, setting, COALESCE(unit,''), source,
		       COALESCE(pending_restart, false), context
		FROM pg_settings
		WHERE name LIKE 'autovacuum%'
		   OR name LIKE 'auto_explain.%'
		   OR name IN (
		      'max_wal_size','min_wal_size','max_slot_wal_keep_size',
		      'checkpoint_completion_target',
		      'wal_compression','wal_level','wal_buffers','checkpoint_timeout',
		      'full_page_writes','shared_buffers','work_mem',
		      'maintenance_work_mem','effective_cache_size','huge_pages',
		      'random_page_cost','default_statistics_target','effective_io_concurrency',
		      'temp_buffers','max_connections','superuser_reserved_connections',
		      'idle_in_transaction_session_timeout','statement_timeout',
		      'tcp_keepalives_idle','tcp_keepalives_interval',
		      'hash_mem_multiplier')`

// configReloptionsSQL lists the tables with per-table storage options
// (autovacuum overrides), temporary tables left out (collection-03).
var configReloptionsSQL = `/* pg_sage */
		SELECT n.nspname, c.relname, c.reloptions::text
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog','information_schema','sage','google_ml')
		  AND c.relkind = 'r'
		  AND ` + notTempRel("c") + `
		  AND c.reloptions IS NOT NULL`

// walPositionSQL reads the WAL position on a primary and on a standby,
// where pg_current_wal_lsn() fails: the last WAL received, else replayed.
// NULL (a standby that has neither yet) is an empty position.
const walPositionSQL = `/* pg_sage */ SELECT (CASE WHEN pg_is_in_recovery()
		THEN COALESCE(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn())
		ELSE pg_current_wal_lsn() END)::text`

const configExtensionsSQL = `/* pg_sage */
		SELECT name FROM pg_available_extensions
		WHERE name IN ('pg_repack','pg_buffercache','pgstattuple','hypopg')`

// configSubRead is one read of the configuration snapshot.
type configSubRead struct {
	name string
	read func(context.Context, catalogQuerier, *ConfigSnapshot) error
}

var configSubReads = []configSubRead{
	{"pg_settings", readConfigSettings},
	{"reloptions", readConfigReloptions},
	{"connection_states", readConnectionStates},
	{"wal_position", readWALPosition},
	{"extensions", readConfigExtensions},
	{"connection_churn", readConnectionChurn},
}

// collectConfigSnapshot reads the advisor's configuration snapshot. Every
// sub-read must succeed: a partial snapshot would present a failed read
// as a fact (a reloptions timeout as "no per-table autovacuum overrides"),
// so any failure, including a row that does not scan, fails the snapshot
// and names the sub-read (collection-05).
func collectConfigSnapshot(ctx context.Context, q catalogQuerier) (*ConfigSnapshot, error) {
	cs := &ConfigSnapshot{}
	for _, sr := range configSubReads {
		if err := sr.read(ctx, q, cs); err != nil {
			return nil, fmt.Errorf("%s: %w", sr.name, err)
		}
	}
	return cs, nil
}

// queryList runs sql and scans every row; a row that does not scan, or an
// error while reading the rows, fails the read.
func queryList[T any](ctx context.Context, q catalogQuerier, sql string,
	scan func(pgx.Rows, *T) error) ([]T, error) {
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func readConfigSettings(ctx context.Context, q catalogQuerier, cs *ConfigSnapshot) (err error) {
	cs.PGSettings, err = queryList(ctx, q, configSettingsSQL,
		func(r pgx.Rows, s *PGSetting) error {
			return r.Scan(&s.Name, &s.Setting, &s.Unit, &s.Source, &s.PendingRestart, &s.Context)
		})
	return err
}

func readConfigReloptions(ctx context.Context, q catalogQuerier, cs *ConfigSnapshot) (err error) {
	cs.TableReloptions, err = queryList(ctx, q, configReloptionsSQL,
		func(r pgx.Rows, t *TableReloption) error {
			return r.Scan(&t.SchemaName, &t.RelName, &t.Reloptions)
		})
	return err
}

func readConnectionStates(ctx context.Context, q catalogQuerier, cs *ConfigSnapshot) (err error) {
	cs.ConnectionStates, err = queryList(ctx, q, connectionStatesSQL,
		func(r pgx.Rows, c *ConnectionState) error {
			return r.Scan(&c.State, &c.Count, &c.AvgDurationSeconds)
		})
	return err
}

func readWALPosition(ctx context.Context, q catalogQuerier, cs *ConfigSnapshot) error {
	var pos *string
	if err := q.QueryRow(ctx, walPositionSQL).Scan(&pos); err != nil {
		return err
	}
	if pos != nil {
		cs.WALPosition = *pos
	}
	return nil
}

func readConfigExtensions(ctx context.Context, q catalogQuerier, cs *ConfigSnapshot) (err error) {
	cs.ExtensionsAvailable, err = queryList(ctx, q, configExtensionsSQL,
		func(r pgx.Rows, name *string) error { return r.Scan(name) })
	return err
}

// readConnectionChurn counts new connections in the last 5 minutes.
func readConnectionChurn(ctx context.Context, q catalogQuerier, cs *ConfigSnapshot) error {
	return q.QueryRow(ctx, connectionChurnSQL).Scan(&cs.ConnectionChurn)
}
