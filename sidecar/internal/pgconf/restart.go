package pgconf

import "strings"

// restartRequired lists the settings whose change only takes effect after
// a PostgreSQL restart (pg_settings.context = 'postmaster'). It is the one
// list pg_sage uses; the advisor and the executor used to keep two lists
// that disagreed and both missed autovacuum_max_workers (G-P0-1).
//
// The list is conservative: autovacuum_max_workers became reloadable in
// PG18, but a change on an older server needs a restart, so it stays.
// Where a live server is available, the executor reads pg_settings.context
// and pending_restart instead of trusting this list.
var restartRequired = map[string]bool{
	"autovacuum_max_workers":         true,
	"huge_pages":                     true,
	"huge_page_size":                 true,
	"max_connections":                true,
	"max_files_per_process":          true,
	"max_locks_per_transaction":      true,
	"max_pred_locks_per_transaction": true,
	"max_prepared_transactions":      true,
	"max_replication_slots":          true,
	"max_wal_senders":                true,
	"max_worker_processes":           true,
	"shared_buffers":                 true,
	"shared_preload_libraries":       true,
	"superuser_reserved_connections": true,
	"track_activity_query_size":      true,
	"wal_buffers":                    true,
	"wal_level":                      true,
}

// RequiresRestart reports whether changing the setting needs a restart.
func RequiresRestart(name string) bool {
	return restartRequired[strings.ToLower(strings.TrimSpace(name))]
}
