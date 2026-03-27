package collector

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func collectConfigSnapshot(ctx context.Context, pool *pgxpool.Pool) (*ConfigSnapshot, error) {
	cs := &ConfigSnapshot{}

	// pg_settings for advisor features
	rows, err := pool.Query(ctx, `
		SELECT name, setting, COALESCE(unit,''), source,
		       COALESCE(pending_restart, false)
		FROM pg_settings
		WHERE name LIKE 'autovacuum%'
		   OR name IN (
		      'max_wal_size','min_wal_size','checkpoint_completion_target',
		      'wal_compression','wal_level','wal_buffers','checkpoint_timeout',
		      'full_page_writes','shared_buffers','work_mem',
		      'maintenance_work_mem','effective_cache_size','huge_pages',
		      'temp_buffers','max_connections','superuser_reserved_connections',
		      'idle_in_transaction_session_timeout','statement_timeout',
		      'tcp_keepalives_idle','tcp_keepalives_interval',
		      'hash_mem_multiplier')`)
	if err != nil {
		return cs, err
	}
	defer rows.Close()
	for rows.Next() {
		var s PGSetting
		if err := rows.Scan(&s.Name, &s.Setting, &s.Unit, &s.Source, &s.PendingRestart); err != nil {
			continue
		}
		cs.PGSettings = append(cs.PGSettings, s)
	}

	// Table reloptions (autovacuum overrides)
	rows2, err := pool.Query(ctx, `
		SELECT n.nspname, c.relname, c.reloptions::text
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog','information_schema','sage','google_ml')
		  AND c.relkind = 'r'
		  AND c.reloptions IS NOT NULL`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var t TableReloption
			if err := rows2.Scan(&t.SchemaName, &t.RelName, &t.Reloptions); err != nil {
				continue
			}
			cs.TableReloptions = append(cs.TableReloptions, t)
		}
	}

	// Connection state distribution
	rows3, err := pool.Query(ctx, `
		SELECT COALESCE(state,'unknown'), count(*)::int,
		       COALESCE(avg(EXTRACT(EPOCH FROM (now() - state_change)))::int, 0)
		FROM pg_stat_activity
		WHERE backend_type = 'client backend'
		  AND datname = current_database()
		GROUP BY state`)
	if err == nil {
		defer rows3.Close()
		for rows3.Next() {
			var c ConnectionState
			if err := rows3.Scan(&c.State, &c.Count, &c.AvgDurationSeconds); err != nil {
				continue
			}
			cs.ConnectionStates = append(cs.ConnectionStates, c)
		}
	}

	// WAL position
	var walPos string
	err = pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&walPos)
	if err == nil {
		cs.WALPosition = walPos
	}

	// Available extensions
	rows4, err := pool.Query(ctx, `
		SELECT name FROM pg_available_extensions
		WHERE name IN ('pg_repack','pg_buffercache','pgstattuple','hypopg')`)
	if err == nil {
		defer rows4.Close()
		for rows4.Next() {
			var name string
			if err := rows4.Scan(&name); err != nil {
				continue
			}
			cs.ExtensionsAvailable = append(cs.ExtensionsAvailable, name)
		}
	}

	// Connection churn (new connections in last 5 minutes)
	var churn int
	err = pool.QueryRow(ctx, `
		SELECT count(*)::int FROM pg_stat_activity
		WHERE backend_start > now() - interval '5 minutes'
		  AND backend_type = 'client backend'`).Scan(&churn)
	if err == nil {
		cs.ConnectionChurn = churn
	}

	return cs, nil
}
